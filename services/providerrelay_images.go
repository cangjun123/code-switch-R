package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const defaultOpenAIImageModel = "gpt-image-2"
const imageDropFieldResponseFormat = "response_format"

type imageProviderCandidate struct {
	kind     string
	provider Provider
}

func (prs *ProviderRelayService) openAIImagesOptionsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		applyOpenAIImagesCORS(c)
		c.Status(http.StatusNoContent)
	}
}

func (prs *ProviderRelayService) openAIImagesCORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		applyOpenAIImagesCORS(c)
		c.Next()
	}
}

func applyOpenAIImagesCORS(c *gin.Context) {
	origin := strings.TrimSpace(c.GetHeader("Origin"))
	if origin == "" {
		origin = "*"
	}

	c.Header("Access-Control-Allow-Origin", origin)
	c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Cache-Control, X-Code-Switch-Key, X-API-Key, X-Requested-With")
	c.Header("Access-Control-Allow-Methods", "POST, OPTIONS")
	c.Header("Access-Control-Max-Age", "86400")
	if origin != "*" {
		c.Header("Vary", "Origin")
	}
}

func (prs *ProviderRelayService) openAIImagesProxyHandler(endpoint string) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))

		model := extractOpenAIImagesModel(c.Request, body)
		streamRequested := openAIImagesStreamRequested(c.Request, body)
		candidates, skipped, err := prs.openAIImageProviderCandidates(model)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load providers"})
			return
		}
		if len(candidates) == 0 {
			message := "no image providers available"
			if model != "" {
				message = fmt.Sprintf("no image providers available for model %q", model)
			}
			c.JSON(http.StatusNotFound, gin.H{
				"error":   message,
				"skipped": skipped,
			})
			return
		}

		query := flattenQuery(c.Request.URL.Query())

		var lastErr error
		var lastProvider string
		var lastKind string
		totalAttempts := 0

		for _, candidate := range candidates {
			totalAttempts++
			lastProvider = candidate.provider.Name
			lastKind = candidate.kind

			effectiveModel := candidate.provider.GetEffectiveModel(model)
			currentBody := body
			currentHeaders := cloneHeaders(c.Request.Header)
			if effectiveModel != model && model != "" && isJSONContentType(c.Request.Header.Get("Content-Type")) {
				modifiedBody, err := ReplaceModelInRequestBody(body, effectiveModel)
				if err != nil {
					lastErr = err
					continue
				}
				currentBody = modifiedBody
			} else if effectiveModel != model && model != "" {
				modifiedBody, contentType, err := replaceMultipartFormField(c.Request.Header.Get("Content-Type"), body, "model", effectiveModel)
				if err != nil {
					lastErr = err
					continue
				}
				if contentType != "" {
					currentBody = modifiedBody
					currentHeaders["Content-Type"] = contentType
				}
			}
			if dropFields := candidate.provider.GetImageDropFields(); len(dropFields) > 0 {
				modifiedBody, contentType, removedFields, err := dropOpenAIImageFields(currentHeaders["Content-Type"], currentBody, dropFields)
				if err != nil {
					lastErr = err
					continue
				}
				if len(removedFields) > 0 {
					currentBody = modifiedBody
					if contentType != "" {
						currentHeaders["Content-Type"] = contentType
					}
					fmt.Printf("[Images] Provider %s 已移除图片请求字段: %v\n", candidate.provider.Name, removedFields)
				}
			}
			if streamRequested {
				currentHeaders["Accept"] = "text/event-stream"
			}

			effectiveEndpoint := resolveOpenAIImageEndpoint(candidate.provider, endpoint)
			start := time.Now()
			ok, err := prs.forwardOpenAIImageRequest(c, candidate.kind, candidate.provider, effectiveEndpoint, query, currentHeaders, currentBody, effectiveModel, streamRequested, candidate.provider.ImageAsyncMode)
			duration := time.Since(start)
			if ok {
				if prs.blacklistService != nil {
					if err := prs.blacklistService.RecordSuccess(candidate.kind, candidate.provider.Name); err != nil {
						fmt.Printf("[Images] 清零失败计数失败: %v\n", err)
					}
				}
				prs.setLastUsedProvider(candidate.kind, candidate.provider.Name)
				fmt.Printf("[Images] ✓ %s/%s 成功 | endpoint=%s | 耗时: %.2fs\n",
					candidate.kind, candidate.provider.Name, effectiveEndpoint, duration.Seconds())
				return
			}

			lastErr = err
			if errors.Is(err, errClientAbort) {
				fmt.Printf("[Images] 客户端中断，停止重试: %s/%s\n", candidate.kind, candidate.provider.Name)
				return
			}
			if prs.blacklistService != nil {
				if err := prs.blacklistService.RecordFailure(candidate.kind, candidate.provider.Name); err != nil {
					fmt.Printf("[Images] 记录失败到黑名单失败: %v\n", err)
				}
			}
			fmt.Printf("[Images] ✗ %s/%s 失败 | endpoint=%s | 错误: %v | 耗时: %.2fs\n",
				candidate.kind, candidate.provider.Name, effectiveEndpoint, err, duration.Seconds())
		}

		errorMsg := "unknown upstream error"
		if lastErr != nil {
			errorMsg = lastErr.Error()
		}
		c.JSON(http.StatusBadGateway, gin.H{
			"error":          fmt.Sprintf("all image providers failed, last error: %s", errorMsg),
			"last_provider":  lastProvider,
			"last_platform":  lastKind,
			"total_attempts": totalAttempts,
		})
	}
}

func (prs *ProviderRelayService) openAIImageProviderCandidates(model string) ([]imageProviderCandidate, int, error) {
	candidates := make([]imageProviderCandidate, 0)
	skipped := 0

	providers, err := prs.providerService.LoadProviders(ProviderKindGPTImage)
	if err != nil {
		return nil, skipped, err
	}

	active := make([]Provider, 0, len(providers))
	for _, provider := range providers {
		if !provider.Enabled || provider.APIURL == "" || provider.APIKey == "" {
			skipped++
			continue
		}
		if errs := provider.ValidateConfiguration(); len(errs) > 0 {
			fmt.Printf("[Images] Provider %s/%s 配置验证失败，已跳过: %v\n", ProviderKindGPTImage, provider.Name, errs)
			skipped++
			continue
		}
		if !providerMayHandleImageModel(ProviderKindGPTImage, provider, model) {
			skipped++
			continue
		}
		if prs.blacklistService != nil {
			if isBlacklisted, until := prs.blacklistService.IsBlacklisted(ProviderKindGPTImage, provider.Name); isBlacklisted {
				fmt.Printf("[Images] Provider %s/%s 已拉黑，过期时间: %v\n", ProviderKindGPTImage, provider.Name, until.Format("15:04:05"))
				skipped++
				continue
			}
		}
		active = append(active, provider)
	}

	for _, provider := range orderProvidersForRelay(ProviderKindGPTImage, active, prs) {
		candidates = append(candidates, imageProviderCandidate{
			kind:     ProviderKindGPTImage,
			provider: provider,
		})
	}

	return candidates, skipped, nil
}

func orderProvidersForRelay(kind string, providers []Provider, prs *ProviderRelayService) []Provider {
	if len(providers) <= 1 {
		return providers
	}

	levelGroups := make(map[int][]Provider)
	for _, provider := range providers {
		level := provider.Level
		if level <= 0 {
			level = 1
		}
		levelGroups[level] = append(levelGroups[level], provider)
	}

	levels := make([]int, 0, len(levelGroups))
	for level := range levelGroups {
		levels = append(levels, level)
	}
	sort.Ints(levels)

	ordered := make([]Provider, 0, len(providers))
	roundRobin := prs != nil && prs.isRoundRobinEnabled()
	for _, level := range levels {
		group := levelGroups[level]
		if roundRobin {
			group = prs.roundRobinOrder(kind, level, group)
		}
		ordered = append(ordered, group...)
	}

	return ordered
}

func providerMayHandleImageModel(kind string, provider Provider, model string) bool {
	hasModelConfig := len(provider.SupportedModels) > 0 || len(provider.ModelMapping) > 0
	if model != "" && hasModelConfig {
		return provider.IsModelSupported(model)
	}
	if model == "" && hasModelConfig {
		return providerLooksImageCapable(provider)
	}
	if strings.EqualFold(kind, ProviderKindGPTImage) {
		return true
	}
	if strings.EqualFold(kind, "codex") {
		return true
	}
	if strings.Contains(strings.ToLower(provider.APIEndpoint), "/images/") {
		return true
	}
	if provider.GetUpstreamProtocol() == UpstreamProtocolOpenAIChat {
		return true
	}
	return false
}

func providerLooksImageCapable(provider Provider) bool {
	if strings.Contains(strings.ToLower(provider.APIEndpoint), "/images/") {
		return true
	}
	for model := range provider.SupportedModels {
		if isLikelyImageModel(model) {
			return true
		}
	}
	for externalModel, internalModel := range provider.ModelMapping {
		if isLikelyImageModel(externalModel) || isLikelyImageModel(internalModel) {
			return true
		}
	}
	return false
}

func extractOpenAIImagesModel(req *http.Request, body []byte) string {
	if req == nil {
		return ""
	}
	if isJSONContentType(req.Header.Get("Content-Type")) {
		return strings.TrimSpace(gjson.GetBytes(body, "model").String())
	}

	mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		req.Body = io.NopCloser(bytes.NewReader(body))
		return ""
	}
	boundary := params["boundary"]
	if boundary == "" {
		req.Body = io.NopCloser(bytes.NewReader(body))
		return ""
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		if part.FormName() != "model" || part.FileName() != "" {
			_ = part.Close()
			continue
		}
		value, _ := io.ReadAll(io.LimitReader(part, 1024))
		_ = part.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
		return strings.TrimSpace(string(value))
	}

	req.Body = io.NopCloser(bytes.NewReader(body))
	return ""
}

func openAIImagesStreamRequested(req *http.Request, body []byte) bool {
	if req == nil {
		return false
	}
	if strings.Contains(strings.ToLower(req.Header.Get("Accept")), "text/event-stream") {
		return true
	}
	if isTruthyString(req.URL.Query().Get("stream")) {
		return true
	}
	if isJSONContentType(req.Header.Get("Content-Type")) {
		stream := gjson.GetBytes(body, "stream")
		return stream.Bool() || isTruthyString(stream.String())
	}

	mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		return false
	}
	boundary := params["boundary"]
	if boundary == "" {
		return false
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err != nil {
			return false
		}
		if part.FormName() != "stream" || part.FileName() != "" {
			_ = part.Close()
			continue
		}
		value, _ := io.ReadAll(io.LimitReader(part, 32))
		_ = part.Close()
		return isTruthyString(string(value))
	}
}

func isTruthyString(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func isJSONContentType(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "application/json")
}

func normalizeImageDropFieldName(field string) string {
	return strings.ToLower(strings.TrimSpace(field))
}

func NormalizeImageDropFields(fields []string) []string {
	if len(fields) == 0 {
		return nil
	}

	normalized := make([]string, 0, len(fields))
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		name := normalizeImageDropFieldName(field)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		normalized = append(normalized, name)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

func dropOpenAIImageFields(contentType string, body []byte, fields []string) ([]byte, string, []string, error) {
	normalizedFields := NormalizeImageDropFields(fields)
	if len(normalizedFields) == 0 {
		return body, "", nil, nil
	}
	if isJSONContentType(contentType) {
		modifiedBody, removedFields, err := dropOpenAIImageJSONFields(body, normalizedFields)
		return modifiedBody, "", removedFields, err
	}
	return dropOpenAIImageMultipartFields(contentType, body, normalizedFields)
}

func dropOpenAIImageJSONFields(body []byte, fields []string) ([]byte, []string, error) {
	req, err := decodeJSONObject(body)
	if err != nil {
		return nil, nil, err
	}

	remaining := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		remaining[field] = struct{}{}
	}

	removedFields := make([]string, 0, len(fields))
	for key := range req {
		normalizedKey := normalizeImageDropFieldName(key)
		if _, ok := remaining[normalizedKey]; !ok {
			continue
		}
		delete(req, key)
		removedFields = append(removedFields, normalizedKey)
		delete(remaining, normalizedKey)
	}
	if len(removedFields) == 0 {
		return body, nil, nil
	}

	result, err := json.Marshal(req)
	if err != nil {
		return nil, nil, err
	}
	return result, removedFields, nil
}

func dropOpenAIImageMultipartFields(contentType string, body []byte, fields []string) ([]byte, string, []string, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		return body, "", nil, nil
	}
	boundary := params["boundary"]
	if boundary == "" {
		return body, "", nil, nil
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)

	removeSet := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		removeSet[field] = struct{}{}
	}
	removedSet := make(map[string]struct{}, len(fields))

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = writer.Close()
			return nil, "", nil, err
		}

		partBody, readErr := io.ReadAll(part)
		if closeErr := part.Close(); readErr == nil {
			readErr = closeErr
		}
		if readErr != nil {
			_ = writer.Close()
			return nil, "", nil, readErr
		}

		fieldName := normalizeImageDropFieldName(part.FormName())
		if _, ok := removeSet[fieldName]; ok {
			removedSet[fieldName] = struct{}{}
			continue
		}

		partWriter, err := writer.CreatePart(cloneMIMEHeader(part.Header))
		if err != nil {
			_ = writer.Close()
			return nil, "", nil, err
		}
		if _, err := partWriter.Write(partBody); err != nil {
			_ = writer.Close()
			return nil, "", nil, err
		}
	}

	if err := writer.Close(); err != nil {
		return nil, "", nil, err
	}
	if len(removedSet) == 0 {
		return body, "", nil, nil
	}

	removedFields := make([]string, 0, len(fields))
	for _, field := range fields {
		if _, ok := removedSet[field]; ok {
			removedFields = append(removedFields, field)
		}
	}
	return out.Bytes(), writer.FormDataContentType(), removedFields, nil
}

func replaceMultipartFormField(contentType string, body []byte, fieldName string, value string) ([]byte, string, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		return body, "", nil
	}
	boundary := params["boundary"]
	if boundary == "" {
		return body, "", nil
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	replaced := false

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = writer.Close()
			return nil, "", err
		}

		partBody, readErr := io.ReadAll(part)
		if closeErr := part.Close(); readErr == nil {
			readErr = closeErr
		}
		if readErr != nil {
			_ = writer.Close()
			return nil, "", readErr
		}

		if part.FormName() == fieldName && part.FileName() == "" {
			if !replaced {
				if err := writer.WriteField(fieldName, value); err != nil {
					_ = writer.Close()
					return nil, "", err
				}
				replaced = true
			}
			continue
		}

		partWriter, err := writer.CreatePart(cloneMIMEHeader(part.Header))
		if err != nil {
			_ = writer.Close()
			return nil, "", err
		}
		if _, err := partWriter.Write(partBody); err != nil {
			_ = writer.Close()
			return nil, "", err
		}
	}

	if !replaced {
		if err := writer.WriteField(fieldName, value); err != nil {
			_ = writer.Close()
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return out.Bytes(), writer.FormDataContentType(), nil
}

func cloneMIMEHeader(header textproto.MIMEHeader) textproto.MIMEHeader {
	cloned := make(textproto.MIMEHeader, len(header))
	for key, values := range header {
		for _, value := range values {
			cloned.Add(key, value)
		}
	}
	return cloned
}

func resolveOpenAIImageEndpoint(provider Provider, defaultEndpoint string) string {
	endpoint := strings.TrimSpace(provider.APIEndpoint)
	if endpoint == "" || !strings.Contains(strings.ToLower(endpoint), "/images/") {
		return defaultEndpoint
	}
	if strings.Contains(strings.ToLower(defaultEndpoint), "/images/") {
		return defaultEndpoint
	}
	return provider.GetEffectiveEndpoint(defaultEndpoint)
}

func (prs *ProviderRelayService) forwardOpenAIImageRequest(
	c *gin.Context,
	kind string,
	provider Provider,
	endpoint string,
	query map[string]string,
	clientHeaders map[string]string,
	bodyBytes []byte,
	model string,
	streamRequested bool,
	asyncMode bool,
) (bool, error) {
	targetURL := joinURL(provider.APIURL, endpoint)
	headers := cloneMap(clientHeaders)
	removeInboundAuthHeaders(headers)
	injectProviderAuthHeaders(headers, provider, false)
	if _, ok := headers["Accept"]; !ok {
		headers["Accept"] = "application/json"
	}
	if asyncMode {
		// 异步模式：创建任务统一走 JSON（即便客户端请求了 stream 也不请求 SSE）
		headers["Accept"] = "application/json"
	} else if streamRequested {
		headers["Accept"] = "text/event-stream"
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return false, err
	}
	for key, value := range headers {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		req.Header.Set(key, value)
	}
	q := req.URL.Query()
	for key, value := range query {
		q.Set(key, value)
	}
	if asyncMode {
		// duomiapi 等异步上游强制要求 ?async=true
		q.Set("async", "true")
	}
	req.URL.RawQuery = q.Encode()

	requestLog := &ReqeustLog{
		Platform: kind,
		Provider: provider.Name,
		Model:    model,
		IsStream: streamRequested,
		ClientIP: clientIPFromRequest(c.Request),
	}
	start := time.Now()
	activeRequestID := defaultActiveRequestTracker.Start(requestLog, start)
	requestLog.ActiveRequestID = activeRequestID
	defer defaultActiveRequestTracker.Finish(activeRequestID)
	defer prs.writeRelayRequestLog(requestLog, start)

	if isClientAbortError(c.Request.Context(), c.Request.Context().Err()) {
		requestLog.SkipLog = true
		return false, errClientAbort
	}

	resp, err := prs.httpClient.Do(req)
	if err != nil {
		if isClientAbortError(ctx, err) {
			requestLog.SkipLog = true
			return false, fmt.Errorf("%w: %v", errClientAbort, err)
		}
		return false, err
	}
	defer resp.Body.Close()
	requestLog.HttpCode = resp.StatusCode

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		if asyncMode {
			return prs.handleAsyncImageResponse(c, ctx, provider, resp, requestLog, start)
		}
		if streamRequested || isOpenAIImageStreamingResponse(resp) {
			if err := streamOpenAIImageResponse(c.Writer, resp, streamRequested, requestLog, start); err != nil {
				if isClientAbortError(c.Request.Context(), err) {
					requestLog.SkipLog = true
				}
				fmt.Printf("[Images] 流式响应转发中断: %v\n", err)
			}
			return true, nil
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, err
		}
		copyOpenAIImageResponseHeaders(c.Writer.Header(), resp.Header)
		c.Data(resp.StatusCode, firstNonEmpty(resp.Header.Get("Content-Type"), "application/json"), body)
		return true, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	if len(body) > 512 {
		body = append(body[:512], []byte("...")...)
	}
	return false, fmt.Errorf("upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

const (
	asyncImagePollInterval = 3 * time.Second // 异步生图任务轮询间隔
	asyncImagePollTimeout  = 10 * time.Minute // 异步生图任务最长等待时间（官方示例约 5 分钟，留足余量）
)

// handleAsyncImageResponse 处理异步生图上游的创建任务响应：
// 读取返回的 {id}，轮询 /v1/tasks/{id} 直到完成，再把结果转成 OpenAI 图片格式返回给客户端。
// 失败/超时返回错误，交由上层走黑名单 + 降级到下一个 Provider 的逻辑。
func (prs *ProviderRelayService) handleAsyncImageResponse(
	c *gin.Context,
	ctx context.Context,
	provider Provider,
	createResp *http.Response,
	requestLog *ReqeustLog,
	start time.Time,
) (bool, error) {
	createBody, err := io.ReadAll(io.LimitReader(createResp.Body, 1<<20))
	if err != nil {
		return false, err
	}

	taskID := strings.TrimSpace(gjson.GetBytes(createBody, "id").String())
	if taskID == "" {
		// 兜底：上游已直接返回完整 OpenAI 响应（含 data 数组），原样透传
		if gjson.GetBytes(createBody, "data").IsArray() {
			copyOpenAIImageResponseHeaders(c.Writer.Header(), createResp.Header)
			c.Data(http.StatusOK, firstNonEmpty(createResp.Header.Get("Content-Type"), "application/json"), createBody)
			requestLog.HttpCode = http.StatusOK
			return true, nil
		}
		snippet := strings.TrimSpace(string(createBody))
		if len(snippet) > 512 {
			snippet = snippet[:512] + "..."
		}
		return false, fmt.Errorf("异步生图创建任务未返回 id: %s", snippet)
	}

	fmt.Printf("[Images] 异步生图任务已提交: id=%s provider=%s，开始轮询 /v1/tasks/%s\n", taskID, provider.Name, taskID)

	state, taskBody, err := prs.pollAsyncImageTask(ctx, provider, taskID)
	if err != nil {
		if isClientAbortError(ctx, err) {
			requestLog.SkipLog = true
			return false, fmt.Errorf("%w: %v", errClientAbort, err)
		}
		return false, fmt.Errorf("异步生图轮询失败 (task=%s): %w", taskID, err)
	}
	if state != "succeeded" {
		return false, fmt.Errorf("异步生图任务未成功 (task=%s, state=%s)", taskID, state)
	}

	openAIBody, err := buildOpenAIImageResponseFromTask(taskBody)
	if err != nil {
		return false, fmt.Errorf("异步生图结果转换失败 (task=%s): %w", taskID, err)
	}

	c.Data(http.StatusOK, "application/json", openAIBody)
	requestLog.HttpCode = http.StatusOK
	fmt.Printf("[Images] ✓ 异步生图任务完成: id=%s provider=%s | 耗时: %.2fs\n", taskID, provider.Name, time.Since(start).Seconds())
	return true, nil
}

// pollAsyncImageTask 轮询 GET /v1/tasks/{id}，直到任务进入终态（succeeded/error）或超时/取消。
func (prs *ProviderRelayService) pollAsyncImageTask(ctx context.Context, provider Provider, taskID string) (string, []byte, error) {
	queryURL := joinURL(provider.APIURL, "/v1/tasks/"+taskID)
	deadline := time.Now().Add(asyncImagePollTimeout)

	ticker := time.NewTicker(asyncImagePollInterval)
	defer ticker.Stop()

	var lastState string
	var lastBody []byte
	for {
		// 任务刚提交时几乎必然 pending，首轮延迟一个间隔再查
		select {
		case <-ctx.Done():
			err := ctx.Err()
			if isClientAbortError(ctx, err) {
				return lastState, lastBody, fmt.Errorf("%w: %v", errClientAbort, err)
			}
			return lastState, lastBody, err
		case <-ticker.C:
		}

		state, body, err := prs.fetchAsyncImageTask(ctx, provider, queryURL)
		if err != nil {
			return lastState, lastBody, err
		}
		lastState, lastBody = state, body

		if state == "succeeded" || state == "error" {
			return state, body, nil
		}
		fmt.Printf("[Images] 异步生图轮询中: task=%s state=%s\n", taskID, state)

		if time.Now().After(deadline) {
			return lastState, lastBody, fmt.Errorf("异步生图轮询超时（%s），任务未完成 (task=%s, last_state=%s)", asyncImagePollTimeout, taskID, lastState)
		}
	}
}

// fetchAsyncImageTask 发起一次 GET /v1/tasks/{id} 查询，返回任务的 state 与原始响应体。
func (prs *ProviderRelayService) fetchAsyncImageTask(ctx context.Context, provider Provider, queryURL string) (string, []byte, error) {
	headers := map[string]string{"Accept": "application/json"}
	removeInboundAuthHeaders(headers)
	injectProviderAuthHeaders(headers, provider, false)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, queryURL, nil)
	if err != nil {
		return "", nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := prs.httpClient.Do(req)
	if err != nil {
		if isClientAbortError(ctx, err) {
			return "", nil, fmt.Errorf("%w: %v", errClientAbort, err)
		}
		return "", nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return "", nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 512 {
			snippet = snippet[:512] + "..."
		}
		return "", nil, fmt.Errorf("查询异步任务失败，上游状态 %d: %s", resp.StatusCode, snippet)
	}

	state := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "state").String()))
	return state, body, nil
}

// buildOpenAIImageResponseFromTask 把 duomiapi 异步任务结果转成 OpenAI 图片响应格式：
// {"created":<ts>,"data":[{"url"|"b64_json":"..."}]}。
func buildOpenAIImageResponseFromTask(taskBody []byte) ([]byte, error) {
	imagesResult := gjson.GetBytes(taskBody, "data.images")
	if !imagesResult.IsArray() {
		return nil, fmt.Errorf("异步任务结果缺少 data.images 数组")
	}

	data := make([]map[string]string, 0)
	for _, img := range imagesResult.Array() {
		item := map[string]string{}
		if b64 := strings.TrimSpace(img.Get("b64_json").String()); b64 != "" {
			item["b64_json"] = b64
		} else if url := strings.TrimSpace(img.Get("url").String()); url != "" {
			item["url"] = url
		} else {
			continue
		}
		data = append(data, item)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("异步任务结果 data.images 中没有可用的图片")
	}

	created := gjson.GetBytes(taskBody, "update_time").Int()
	if created == 0 {
		created = gjson.GetBytes(taskBody, "create_time").Int()
	}

	return json.Marshal(map[string]interface{}{
		"created": created,
		"data":    data,
	})
}

func isOpenAIImageStreamingResponse(resp *http.Response) bool {	if resp == nil {
		return false
	}
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
}

func streamOpenAIImageResponse(w http.ResponseWriter, resp *http.Response, forceSSE bool, requestLog *ReqeustLog, start time.Time) error {
	if resp == nil {
		return fmt.Errorf("empty upstream response")
	}

	copyStreamingResponseHeaders(w.Header(), resp.Header)
	if forceSSE && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	w.WriteHeader(resp.StatusCode)

	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			markFirstTokenDuration(requestLog, start)
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("%w: %v", errClientAbort, writeErr)
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return fmt.Errorf("error streaming upstream image response: %w", readErr)
		}
	}
}

func injectProviderAuthHeaders(headers map[string]string, provider Provider, anthropicVersion bool) {
	authType := strings.ToLower(strings.TrimSpace(provider.ConnectivityAuthType))
	switch authType {
	case "x-api-key":
		headers["x-api-key"] = provider.APIKey
		if anthropicVersion {
			headers["anthropic-version"] = "2023-06-01"
		}
	case "", "bearer":
		headers["Authorization"] = fmt.Sprintf("Bearer %s", provider.APIKey)
	default:
		headerName := strings.TrimSpace(provider.ConnectivityAuthType)
		if headerName == "" || strings.EqualFold(headerName, "custom") {
			headerName = "Authorization"
		}
		headers[headerName] = provider.APIKey
	}
}

func copyOpenAIImageResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		switch strings.ToLower(key) {
		case "content-length", "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
			"te", "trailer", "transfer-encoding", "upgrade":
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func (prs *ProviderRelayService) writeRelayRequestLog(requestLog *ReqeustLog, start time.Time) {
	if requestLog == nil {
		return
	}
	requestLog.DurationSec = time.Since(start).Seconds()
	if requestLog.SkipLog {
		return
	}
	if GlobalDBQueueLogs == nil {
		fmt.Printf("⚠️  写入 request_log 失败: 队列未初始化\n")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := GlobalDBQueueLogs.ExecBatchCtx(ctx, `
		INSERT INTO request_log (
			platform, model, provider, http_code,
			input_tokens, output_tokens, cache_create_tokens, cache_read_tokens,
			reasoning_tokens, is_stream, duration_sec, first_token_duration_sec, client_ip,
			is_degraded, resend_count
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		requestLog.Platform,
		requestLog.Model,
		requestLog.Provider,
		requestLog.HttpCode,
		requestLog.InputTokens,
		requestLog.OutputTokens,
		requestLog.CacheCreateTokens,
		requestLog.CacheReadTokens,
		requestLog.ReasoningTokens,
		boolToInt(requestLog.IsStream),
		requestLog.DurationSec,
		requestLog.FirstTokenDurationSec,
		requestLog.ClientIP,
		boolToInt(requestLog.IsDegraded),
		requestLog.ResendCount,
	)
	if err != nil {
		fmt.Printf("写入 request_log 失败: %v\n", err)
	}
}

func (prs *ProviderRelayService) appendConfiguredImageModelsToModelList(body []byte, kind string) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	if _, ok := raw["data"]; !ok {
		return body
	}

	var payload struct {
		Object string                   `json:"object,omitempty"`
		Data   []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	if payload.Object == "" {
		payload.Object = "list"
	}

	seen := make(map[string]bool, len(payload.Data)+1)
	for _, item := range payload.Data {
		if id, _ := item["id"].(string); id != "" {
			seen[id] = true
		}
	}

	for _, model := range prs.configuredImageModels(kind) {
		if seen[model] {
			continue
		}
		payload.Data = append(payload.Data, map[string]interface{}{
			"id":       model,
			"object":   "model",
			"created":  0,
			"owned_by": "code-switch",
		})
		seen[model] = true
	}

	if len(payload.Data) == 0 || seen[defaultOpenAIImageModel] || !prs.hasOpenAIImageProvider(defaultOpenAIImageModel) {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return body
		}
		return encoded
	}

	payload.Data = append(payload.Data, map[string]interface{}{
		"id":       defaultOpenAIImageModel,
		"object":   "model",
		"created":  0,
		"owned_by": "code-switch",
	})

	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

func (prs *ProviderRelayService) writeConfiguredImageModelsResponse(c *gin.Context, kind string) bool {
	payload := map[string]interface{}{
		"object": "list",
		"data":   []map[string]interface{}{},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	body = prs.appendConfiguredImageModelsToModelList(body, kind)
	if !strings.Contains(string(body), `"id"`) {
		return false
	}
	c.Data(http.StatusOK, "application/json", body)
	return true
}

func (prs *ProviderRelayService) hasOpenAIImageProvider(model string) bool {
	candidates, _, err := prs.openAIImageProviderCandidates(model)
	return err == nil && len(candidates) > 0
}

func (prs *ProviderRelayService) configuredImageModels(preferredKind string) []string {
	seenModel := map[string]bool{}
	models := make([]string, 0)
	providers, err := prs.providerService.LoadProviders(ProviderKindGPTImage)
	if err != nil {
		return models
	}
	for _, provider := range providers {
		if !provider.Enabled || provider.APIURL == "" || provider.APIKey == "" {
			continue
		}
		for model := range provider.SupportedModels {
			if isLikelyImageModel(model) && !seenModel[model] {
				models = append(models, model)
				seenModel[model] = true
			}
		}
		for externalModel := range provider.ModelMapping {
			if isLikelyImageModel(externalModel) && !seenModel[externalModel] {
				models = append(models, externalModel)
				seenModel[externalModel] = true
			}
		}
	}
	sort.Strings(models)
	return models
}

func isLikelyImageModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	imageMarkers := []string{
		"gpt-image",
		"dall-e",
		"dalle",
		"imagen",
		"image",
		"stable-diffusion",
		"sdxl",
		"flux",
		"midjourney",
	}
	for _, marker := range imageMarkers {
		if strings.Contains(model, marker) {
			return true
		}
	}
	return false
}
