package services

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"codeswitch/services/modeltrace"
)

// ModelTraceService 模型真伪检测服务：
// 向指定 provider 的指定模型发送一条数值生成挑战，从数字分布指纹归因模型身份，
// top-1 与用户选择的模型不一致时判定为疑似偷换。
// 检测过程中的阶段进度通过 EventHub 以 "modeltrace:progress" 事件推送（SSE），
// 模型流式生成的文本片段以 "modeltrace:stream" 事件实时推送。
type ModelTraceService struct {
	providerService *ProviderService

	emitter EventEmitter

	client *http.Client
}

// NewModelTraceService 创建模型真伪检测服务
func NewModelTraceService(providerService *ProviderService) *ModelTraceService {
	return &ModelTraceService{
		providerService: providerService,
		client: &http.Client{
			Timeout: 4 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				IdleConnTimeout:     30 * time.Second,
				DisableCompression:  true,
				MaxIdleConnsPerHost: 5,
			},
		},
	}
}

// SetEventEmitter 设置事件发送器（转发进度到 SSE/web 前端）
func (mts *ModelTraceService) SetEventEmitter(emitter EventEmitter) {
	mts.emitter = emitter
}

// Start / Stop 满足 Wails 生命周期接口
func (mts *ModelTraceService) Start() error { return nil }
func (mts *ModelTraceService) Stop() error  { return nil }

// ModelTraceModelOption 指纹库覆盖的模型（前端选择器数据源）
type ModelTraceModelOption struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Family      string `json:"family"`
	FamilyName  string `json:"familyName"`
}

// GetSupportedModels 返回指纹库覆盖的模型列表
func (mts *ModelTraceService) GetSupportedModels() ([]ModelTraceModelOption, error) {
	bank, err := modeltrace.LoadBank()
	if err != nil {
		return nil, err
	}
	options := bank.SupportedModels()
	result := make([]ModelTraceModelOption, 0, len(options))
	for _, option := range options {
		result = append(result, ModelTraceModelOption(option))
	}
	return result, nil
}

// ModelTraceProbability 单模型归因概率
type ModelTraceProbability struct {
	Model             string  `json:"model"`
	DisplayName       string  `json:"displayName"`
	Family            string  `json:"family"`
	FamilyName        string  `json:"familyName"`
	Probability       float64 `json:"probability"`
	ProfileSimilarity float64 `json:"profileSimilarity"`
}

// ModelTraceResult 鉴伪结果
type ModelTraceResult struct {
	Success          bool                    `json:"success"`
	Message          string                  `json:"message"`
	ExpectedModel    string                  `json:"expectedModel"`
	ExpectedMatched  bool                    `json:"expectedMatched"`
	TopModel         string                  `json:"topModel"`
	TopModelName     string                  `json:"topModelName"`
	TopProbability   float64                 `json:"topProbability"`
	Verdict          string                  `json:"verdict"` // match / mismatch / error
	ValidNumberCount int                     `json:"validNumberCount"`
	Attempts         int                     `json:"attempts"`
	LatencyMs        int                     `json:"latencyMs"`
	Probabilities    []ModelTraceProbability `json:"probabilities"`
	RawOutput        string                  `json:"rawOutput,omitempty"`
}

// ModelTraceProgressEvent 鉴伪过程进度事件（modeltrace:progress）
type ModelTraceProgressEvent struct {
	// SessionID 一次 VerifyProviderModel 调用的唯一标识，前端锁定会话后据此过滤
	SessionID string `json:"sessionId"`
	// ProviderID / ExpectedModel 用于前端在收到首个事件时归属会话
	//（RPC 调用是阻塞的，前端在调用返回前无法得知 SessionID，
	// 只能用请求参数匹配第一个事件，再锁定其 sessionId）
	ProviderID    int64  `json:"providerId"`
	ExpectedModel string `json:"expectedModel"`
	// Stage 阶段：sending / received / analyzing / retrying / done / failed
	Stage string `json:"stage"`
	// Attempt 当前尝试序号（1 起）
	Attempt int `json:"attempt"`
	// MaxAttempts 尝试上限
	MaxAttempts int `json:"maxAttempts"`
	// Detail 人类可读的阶段描述
	Detail string `json:"detail"`
	// ElapsedMs 距本次检测开始的毫秒数
	ElapsedMs int `json:"elapsedMs"`
}

// emitProgress 推送进度事件（emitter 未注入时静默跳过）
func (mts *ModelTraceService) emitProgress(sessionID string, providerID int64, expectedModel, stage string, attempt int, start time.Time, detail string) {
	if mts.emitter == nil {
		return
	}
	mts.emitter.Emit("modeltrace:progress", ModelTraceProgressEvent{
		SessionID:     sessionID,
		ProviderID:    providerID,
		ExpectedModel: expectedModel,
		Stage:         stage,
		Attempt:       attempt,
		MaxAttempts:   maxVerifyAttempts,
		Detail:        detail,
		ElapsedMs:     int(time.Since(start).Milliseconds()),
	})
}

// ModelTraceStreamEvent 流式生成片段事件（modeltrace:stream）
type ModelTraceStreamEvent struct {
	SessionID string `json:"sessionId"`
	// Chunk 本次到达的回答文本片段（前端追加展示）
	Chunk string `json:"chunk"`
	// TotalChars 截至当前已累计收到的字符数
	TotalChars int `json:"totalChars"`
}

// streamChunker 把流式回答按节流批量推送：上游 SSE 每个 token 一条事件太频繁，
// EventHub 的广播会刷爆 SSE 通道。攒满 minChunk 或超过 flushInterval 才发一条。
type streamChunker struct {
	sessionID string
	emitter   EventEmitter

	pending       strings.Builder
	totalChars    int
	lastFlush     time.Time
	minChunk      int
	flushInterval time.Duration
}

func newStreamChunker(sessionID string, emitter EventEmitter) *streamChunker {
	return &streamChunker{
		sessionID:     sessionID,
		emitter:       emitter,
		minChunk:      24,
		flushInterval: 250 * time.Millisecond,
	}
}

// Add 追加一段文本；满足节流条件时自动推送
func (s *streamChunker) Add(text string) {
	if s.emitter == nil || text == "" {
		return
	}
	s.pending.WriteString(text)
	s.totalChars += len([]rune(text))
	if s.pending.Len() >= s.minChunk && time.Since(s.lastFlush) >= s.flushInterval {
		s.Flush()
	}
}

// Flush 立即推送当前积压（可为空）
func (s *streamChunker) Flush() {
	if s.emitter == nil {
		return
	}
	chunk := s.pending.String()
	if chunk == "" {
		return
	}
	s.pending.Reset()
	s.lastFlush = time.Now()
	s.emitter.Emit("modeltrace:stream", ModelTraceStreamEvent{
		SessionID:  s.sessionID,
		Chunk:      chunk,
		TotalChars: s.totalChars,
	})
}

// VerifyProviderModel 对指定 provider + 模型执行一次指纹鉴伪。
// 平台/模型不一致（疑似偷换）时 Success=true 且 ExpectedMatched=false。
func (mts *ModelTraceService) VerifyProviderModel(
	platform string,
	providerID int64,
	expectedModel string,
) ModelTraceResult {
	start := time.Now()

	expectedModel = strings.TrimSpace(expectedModel)
	if expectedModel == "" {
		return ModelTraceResult{Verdict: "error", Message: "未指定待检测模型"}
	}
	bank, err := modeltrace.LoadBank()
	if err != nil {
		return ModelTraceResult{Verdict: "error", Message: err.Error()}
	}
	if !bank.ContainsModel(expectedModel) {
		return ModelTraceResult{
			Verdict: "error",
			Message: fmt.Sprintf("模型 %s 不在指纹库覆盖范围内（仅支持 GPT 5.x/6 与 Claude 4.5+/5 系列）", expectedModel),
		}
	}

	providers, err := mts.providerService.LoadProviders(platform)
	if err != nil {
		return ModelTraceResult{Verdict: "error", Message: fmt.Sprintf("读取供应商失败: %v", err)}
	}
	var provider *Provider
	for i := range providers {
		if providers[i].ID == providerID {
			provider = &providers[i]
			break
		}
	}
	if provider == nil {
		return ModelTraceResult{Verdict: "error", Message: "未找到指定供应商"}
	}

	// 模型映射生效：外部模型名 -> provider 实际请求的内部模型名
	apiModel := provider.GetEffectiveModel(expectedModel)
	if apiModel != expectedModel && !bank.ContainsModel(apiModel) {
		return ModelTraceResult{
			Verdict:       "error",
			ExpectedModel: expectedModel,
			Message: fmt.Sprintf(
				"该供应商把 %s 映射为 %s，映射目标不在指纹库覆盖范围内，无法比对真伪",
				expectedModel, apiModel),
		}
	}

	// 每次调用一个会话 ID，前端据此过滤进度事件
	sessionID := fmt.Sprintf("mt-%d-%s", providerID, modeltrace.NewSessionToken())
	if apiModel != expectedModel {
		mts.emitProgress(sessionID, providerID, expectedModel, "sending", 1, start,
			fmt.Sprintf("已应用模型映射 %s → %s，正在发起检测…", expectedModel, apiModel))
	} else {
		mts.emitProgress(sessionID, providerID, expectedModel, "sending", 1, start,
			fmt.Sprintf("正在对 %s（%s）发起检测…", provider.Name, expectedModel))
	}

	result, attempts, rawOutput, verifyErr := mts.verifyWithRetries(sessionID, start, providerID, expectedModel, provider, platform, apiModel, bank)
	result.ExpectedModel = expectedModel
	result.Attempts = attempts
	result.LatencyMs = int(time.Since(start).Milliseconds())
	if rawOutput != "" {
		result.RawOutput = rawOutput
	}
	if verifyErr != nil {
		result.Success = false
		result.Verdict = "error"
		result.Message = verifyErr.Error()
		mts.emitProgress(sessionID, providerID, expectedModel, "failed", attempts, start, verifyErr.Error())
		return result
	}

	// 比对对象是映射后的 apiModel（provider 实际转发的模型）。
	// 与外部名 expectedModel 比对会把用户自己配置的映射误判为"偷换"。
	result.ExpectedMatched = result.TopModel == apiModel
	if result.ExpectedMatched {
		result.Verdict = "match"
		mts.emitProgress(sessionID, providerID, expectedModel, "done", attempts, start, "检测完成：身份一致")
	} else {
		result.Verdict = "mismatch"
		mts.emitProgress(sessionID, providerID, expectedModel, "done", attempts, start, "检测完成：疑似偷换")
	}
	return result
}

const (
	// maxVerifyAttempts 单次鉴伪最多尝试的挑战数（目标 1 条有效回答）
	maxVerifyAttempts = 3
	// verifyTotalBudget 整个鉴伪流程的总时间预算（含全部重试），
	// 避免单次超时 × 重试次数导致长时间不可取消的阻塞
	verifyTotalBudget = 4 * time.Minute
	// perAttemptTimeout 单次挑战请求的超时（受总预算约束）
	perAttemptTimeout = 2 * time.Minute
)

// verifyWithRetries 发挑战直到拿到一条有效回答或用尽尝试次数 / 总预算。
// 过程中的每个阶段通过 emitProgress 推送实时进度。
func (mts *ModelTraceService) verifyWithRetries(sessionID string, start time.Time, providerID int64, expectedModel string, provider *Provider, platform, apiModel string, bank *modeltrace.Bank) (ModelTraceResult, int, string, error) {
	challenges := modeltrace.GenerateChallenges(maxVerifyAttempts)
	var lastRaw string
	var lastErr error
	attempts := 0
	budgetCtx, cancel := context.WithTimeout(context.Background(), verifyTotalBudget)
	defer cancel()
	for _, challenge := range challenges {
		attempts++
		if attempts > 1 {
			mts.emitProgress(sessionID, providerID, expectedModel, "retrying", attempts, start,
				fmt.Sprintf("第 %d 次回答无效（%v），正在发送新的挑战…", attempts-1, lastErr))
		}
		mts.emitProgress(sessionID, providerID, expectedModel, "sending", attempts, start,
			"挑战已发出，正在等待模型生成约 300 个随机整数（通常 30 秒到 2 分钟）…")
		streamer := newStreamChunker(sessionID, mts.emitter)
		text, err := mts.requestChallenge(budgetCtx, provider, platform, apiModel, challenge.Prompt, streamer)
		if err != nil {
			lastErr = err
			if budgetCtx.Err() != nil {
				mts.emitProgress(sessionID, providerID, expectedModel, "failed", attempts, start, "总时间预算耗尽，停止重试")
				break // 总预算耗尽，停止重试
			}
			continue
		}
		lastRaw = text
		mts.emitProgress(sessionID, providerID, expectedModel, "received", attempts, start,
			fmt.Sprintf("已收到回答（%d 字符），正在解析数字序列并计算指纹…", len([]rune(text))))
		outputs := []modeltrace.Output{{
			Text:          text,
			ExpectedCount: challenge.ExpectedCount,
		}}
		analysis, err := modeltrace.Analyze(outputs, "", bank)
		if err != nil {
			lastErr = fmt.Errorf("回答无效（有效数字不足）: %w", err)
			continue
		}
		probabilities := make([]ModelTraceProbability, 0, len(analysis.Results))
		for _, item := range analysis.Results {
			probabilities = append(probabilities, ModelTraceProbability{
				Model:             item.Model,
				DisplayName:       item.DisplayName,
				Family:            item.Family,
				FamilyName:        item.FamilyName,
				Probability:       item.Probability,
				ProfileSimilarity: item.ProfileSimilarity,
			})
		}
		return ModelTraceResult{
			Success:          true,
			Message:          "",
			TopModel:         analysis.TopModel,
			TopModelName:     analysis.PredictionName,
			TopProbability:   analysis.Probability,
			ValidNumberCount: analysis.ValidNumberCount,
			Probabilities:    probabilities,
		}, attempts, lastRaw, nil
	}
	if lastErr == nil {
		lastErr = errors.New("未能获得有效回答")
	}
	return ModelTraceResult{}, attempts, lastRaw, lastErr
}

// completionAuthHeader 按认证方式返回 (header 名, header 值)。
// 语义与连通性测试一致：空值/bearer → Authorization: Bearer <key>；
// x-api-key → x-api-key: <key>；其余（含 "custom" 与自定义 Header 名）→
// 原样 Header 名 + 无前缀 key。
func completionAuthHeader(authType, apiKey string) (string, string) {
	authType = strings.TrimSpace(authType)
	switch strings.ToLower(authType) {
	case "x-api-key":
		return "x-api-key", apiKey
	case "bearer", "":
		return "Authorization", "Bearer " + apiKey
	default:
		// custom / 自定义 Header 名：与连通性测试一致，无 Bearer 前缀
		headerName := authType
		if headerName == "" || strings.EqualFold(headerName, "custom") {
			headerName = "Authorization"
		}
		return headerName, apiKey
	}
}

// anthropicRequest / openaiChatRequest 构造挑战请求体
type challengeMessages struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func buildAnthropicChallengeBody(model, prompt string, stream bool) ([]byte, error) {
	content, _ := json.Marshal(prompt)
	body := map[string]interface{}{
		"model":      model,
		"max_tokens": 4096,
		"messages": []challengeMessages{
			{Role: "user", Content: content},
		},
	}
	if stream {
		body["stream"] = true
	}
	return json.Marshal(body)
}

func buildOpenAIChallengeBody(model, prompt string, stream bool) ([]byte, error) {
	content, _ := json.Marshal(prompt)
	body := map[string]interface{}{
		"model": model,
		"messages": []challengeMessages{
			{Role: "user", Content: content},
		},
	}
	if stream {
		body["stream"] = true
	}
	return json.Marshal(body)
}

// buildResponsesChallengeBody 构造 OpenAI Responses API 格式的挑战请求（input 字段）
func buildResponsesChallengeBody(model, prompt string, stream bool) ([]byte, error) {
	body := map[string]interface{}{
		"model": model,
		"input": []map[string]interface{}{
			{
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": prompt},
				},
			},
		},
	}
	if stream {
		body["stream"] = true
	}
	return json.Marshal(body)
}

// wafBlockMarkers Cloudflare/WAF 拦截页特征
var wafBlockMarkers = []string{"cloudflare", "just a moment", "cf-ray", "access denied", "attention required"}

func looksLikeWAFBlock(text string) bool {
	lowered := strings.ToLower(text)
	for _, marker := range wafBlockMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// compactUpstreamError 压缩上游错误信息用于展示
func compactUpstreamError(details string) string {
	text := strings.TrimSpace(details)
	if text == "" {
		return "上游接口返回错误"
	}
	if looksLikeWAFBlock(text) {
		return "请求被上游网关拦截（Cloudflare/WAF 拦截页），请确认 API 地址指向 API 端点且 Key 有效"
	}
	if strings.HasPrefix(text, "<") || (!strings.Contains(text, "{") && strings.Contains(strings.ToLower(text), "html")) {
		if len(text) > 200 {
			return text[:200]
		}
		return text
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(text), &payload); err == nil {
		if errorObj, ok := payload["error"].(map[string]interface{}); ok {
			if message, ok := errorObj["message"].(string); ok && message != "" {
				return message
			}
			if compact, err := json.Marshal(errorObj); err == nil {
				return string(compact)
			}
		}
		if errorStr, ok := payload["error"].(string); ok && errorStr != "" {
			return errorStr
		}
		if compact, err := json.Marshal(payload); err == nil && len(compact) <= 500 {
			return string(compact)
		}
	}
	if len(text) > 500 {
		return text[:500]
	}
	return text
}

// requestChallenge 向 provider 发送一条挑战并返回回答文本。
// 端点/认证解析与连通性测试共用同一套规则；请求体按端点选择
// Anthropic / OpenAI Chat / OpenAI Responses 三种格式。
// 优先以 SSE 流式请求并把生成片段经 streamer 实时推送（streamer 为 nil 时退化为非流式）。
// 不传 temperature（用渠道默认，测线上真实状态）。
func (mts *ModelTraceService) requestChallenge(parent context.Context, provider *Provider, platform, apiModel, prompt string, streamer *streamChunker) (string, error) {
	endpoint := resolveConnectivityEndpoint(provider, platform)
	protocol := provider.ResolveUpstreamProtocol(endpoint)
	targetURL := joinURL(provider.APIURL, endpoint)

	var reqBody []byte
	var err error
	isAnthropic := protocol == UpstreamProtocolAnthropic
	isResponses := !isAnthropic && strings.Contains(strings.ToLower(endpoint), "/responses")
	useStream := streamer != nil
	switch {
	case isAnthropic:
		reqBody, err = buildAnthropicChallengeBody(apiModel, prompt, useStream)
	case isResponses:
		reqBody, err = buildResponsesChallengeBody(apiModel, prompt, useStream)
	default:
		reqBody, err = buildOpenAIChallengeBody(apiModel, prompt, useStream)
	}
	if err != nil {
		return "", fmt.Errorf("构造请求失败: %w", err)
	}

	authType := strings.TrimSpace(provider.ConnectivityAuthType)
	if authType == "" {
		authType = "bearer"
	}

	ctx, cancel := context.WithTimeout(parent, perAttemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("创建请求失败: %w", err)
	}
	if useStream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("Content-Type", "application/json")
	// 伪装成真实客户端 UA，避免被 Cloudflare/WAF 拦截
	req.Header.Set("User-Agent", "Codex Desktop/0.147.0-alpha.1.2 (Windows 10.0.26200; x86_64) unknown (codex_exec; 0.147.0-alpha.1.2)")
	if provider.APIKey != "" {
		headerName, headerValue := completionAuthHeader(authType, provider.APIKey)
		req.Header.Set(headerName, headerValue)
		if isAnthropic {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	}

	resp, err := mts.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("无法连接接口: %w", err)
	}
	defer resp.Body.Close()

	// 流式请求被拒绝（部分网关不支持 stream 参数）时降级为非流式重试
	if useStream && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		if fallback, fbErr := mts.requestChallenge(parent, provider, platform, apiModel, prompt, nil); fbErr == nil {
			return fallback, nil
		}
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, compactUpstreamError(string(body)))
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, compactUpstreamError(string(body)))
	}

	if useStream && strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		text, streamErr := consumeSSEStream(resp.Body, streamer)
		if streamErr != nil {
			return "", streamErr
		}
		// 流里没拿到任何文本（如上游忽略 stream 返回了非 SSE 形态但 Content-Type 撒谎），走非流式重试
		if text != "" {
			return text, nil
		}
		resp.Body.Close()
		return mts.requestChallenge(parent, provider, platform, apiModel, prompt, nil)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}
	return extractCompletionText(body, isAnthropic)
}

// consumeSSEStream 逐行解析上游 SSE 流，把文本片段喂给 streamer，
// 返回拼合后的完整文本。流中透传的截断/拒答终止原因同样拒绝该回答。
func consumeSSEStream(reader io.Reader, streamer *streamChunker) (string, error) {
	var full strings.Builder
	var lastStopReason string
	br := bufio.NewReaderSize(reader, 16<<10)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			if after, ok := strings.CutPrefix(trimmed, "data:"); ok {
				payload := strings.TrimSpace(after)
				if payload == "" || payload == "[DONE]" {
					if payload == "[DONE]" {
						goto done
					}
				} else if text, stop, extractErr := extractStreamDelta(payload); extractErr == nil {
					if text != "" {
						full.WriteString(text)
						streamer.Add(text)
					}
					if stop != "" {
						lastStopReason = stop
						goto done
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			streamer.Flush()
			return full.String(), fmt.Errorf("读取流式响应失败: %w", err)
		}
	}
done:
	streamer.Flush()
	switch lastStopReason {
	case "refusal":
		return "", errors.New("模型拒绝生成，本次回答不计入")
	case "max_tokens", "length", "content_filter":
		return "", fmt.Errorf("回答未正常完成（%s），本次回答不计入", lastStopReason)
	}
	return full.String(), nil
}

// extractStreamDelta 从单条 SSE data 中提取增量文本与终止原因。
// 兼容 Anthropic（content_block_delta）与 OpenAI Chat/Responses（delta/output_text）三种形态。
func extractStreamDelta(payload string) (text, stopReason string, err error) {
	var raw struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
		Choices []struct {
			Delta struct {
				Content json.RawMessage `json:"content"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		// OpenAI Responses 形态
		Response *struct {
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		} `json:"response"`
		// Anthropic message_delta / 完成事件
		StopReason string `json:"stop_reason"`
	}
	if jsonErr := json.Unmarshal([]byte(payload), &raw); jsonErr != nil {
		return "", "", jsonErr
	}
	// Anthropic: content_block_delta
	if raw.Type == "content_block_delta" && (raw.Delta.Type == "text_delta" || raw.Delta.Text != "") {
		return raw.Delta.Text, "", nil
	}
	// Anthropic: message_delta 携带终止原因
	if raw.Type == "message_delta" && raw.StopReason != "" {
		return "", raw.StopReason, nil
	}
	// OpenAI Chat: choices[0].delta.content（字符串或分段数组）
	if len(raw.Choices) > 0 {
		if content := raw.Choices[0].Delta.Content; len(content) > 0 {
			if text := rawContentText(content); text != "" {
				return text, "", nil
			}
		}
		if raw.Choices[0].FinishReason != nil && *raw.Choices[0].FinishReason != "" {
			return "", *raw.Choices[0].FinishReason, nil
		}
	}
	// OpenAI Responses: type == response.output_text.delta 的 data 顶层是文本？官方形态为
	// {"type":"response.output_text.delta","delta":"..."}——补一个宽松字段
	var responsesDelta struct {
		Type  string          `json:"type"`
		Delta json.RawMessage `json:"delta"`
	}
	_ = json.Unmarshal([]byte(payload), &responsesDelta)
	if responsesDelta.Type == "response.output_text.delta" {
		var s string
		if json.Unmarshal(responsesDelta.Delta, &s) == nil {
			return s, "", nil
		}
	}
	if responsesDelta.Type == "response.incomplete" && raw.Response != nil && raw.Response.IncompleteDetails != nil {
		return "", raw.Response.IncompleteDetails.Reason, nil
	}
	if responsesDelta.Type == "response.completed" && raw.Response != nil {
		if raw.Response.IncompleteDetails != nil && raw.Response.IncompleteDetails.Reason != "" {
			return "", raw.Response.IncompleteDetails.Reason, nil
		}
		return "", "stop", nil
	}
	return "", "", nil
}

// rawContentText 解析 chat delta 的 content 字段（字符串或分段数组）
func rawContentText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	if content[0] == '"' {
		var s string
		if json.Unmarshal(content, &s) == nil {
			return s
		}
		return ""
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &parts) == nil {
		var text strings.Builder
		for _, part := range parts {
			text.WriteString(part.Text)
		}
		return text.String()
	}
	return ""
}

// extractCompletionText 从上游响应中提取回答文本并过滤截断/拒答
func extractCompletionText(body []byte, isAnthropic bool) (string, error) {
	if isAnthropic {
		var payload struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			StopReason string `json:"stop_reason"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return "", fmt.Errorf("响应解析失败: %v", err)
		}
		var text strings.Builder
		for _, block := range payload.Content {
			if block.Type == "text" {
				text.WriteString(block.Text)
			}
		}
		switch payload.StopReason {
		case "refusal":
			return "", errors.New("模型拒绝生成，本次回答不计入")
		case "max_tokens":
			return "", errors.New("回答因 max_tokens 截断，本次回答不计入")
		}
		if text.Len() == 0 {
			return "", errors.New("上游响应中没有文本内容")
		}
		return text.String(), nil
	}

	var payload struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("响应解析失败: %v", err)
	}
	if len(payload.Choices) == 0 {
		// Responses API 形态（以及部分把 /responses 形态原样返回的 chat 上游）
		return extractResponsesText(body)
	}
	choice := payload.Choices[0]
	switch choice.FinishReason {
	case "length", "content_filter":
		return "", fmt.Errorf("回答未正常完成（%s），本次回答不计入", choice.FinishReason)
	}
	content := choice.Message.Content
	if len(content) > 0 && content[0] == '"' {
		var s string
		if err := json.Unmarshal(content, &s); err == nil && s != "" {
			return s, nil
		}
	}
	// content 为分段数组形态
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err == nil {
		var text strings.Builder
		for _, part := range parts {
			text.WriteString(part.Text)
		}
		if text.Len() > 0 {
			return text.String(), nil
		}
	}
	var asString string
	if err := json.Unmarshal(content, &asString); err == nil {
		return asString, nil
	}
	return "", errors.New("无法从响应中提取文本")
}

// extractResponsesText 从 OpenAI Responses API 形态的响应中提取文本，
// 并按 status / incomplete 过滤截断与拒答
func extractResponsesText(body []byte) (string, error) {
	var payload struct {
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []struct {
			Content []struct {
				Type       string          `json:"type"`
				Text       string          `json:"text"`
				OutputText json.RawMessage `json:"output_text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("响应解析失败: %v", err)
	}
	if payload.IncompleteDetails != nil && payload.IncompleteDetails.Reason != "" {
		return "", fmt.Errorf("回答未正常完成（%s），本次回答不计入", payload.IncompleteDetails.Reason)
	}
	var text strings.Builder
	for _, output := range payload.Output {
		for _, part := range output.Content {
			if part.Type != "output_text" && part.Type != "text" {
				continue
			}
			if part.Text != "" {
				text.WriteString(part.Text)
			} else if len(part.OutputText) > 0 {
				var s string
				if err := json.Unmarshal(part.OutputText, &s); err == nil {
					text.WriteString(s)
				}
			}
		}
	}
	if text.Len() == 0 {
		return "", errors.New("上游响应中没有文本内容")
	}
	return text.String(), nil
}
