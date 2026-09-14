package services

import (
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
type ModelTraceService struct {
	providerService *ProviderService

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

	result, attempts, rawOutput, verifyErr := mts.verifyWithRetries(provider, platform, apiModel, bank)
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
		return result
	}

	result.ExpectedMatched = result.TopModel == expectedModel
	if result.ExpectedMatched {
		result.Verdict = "match"
	} else {
		result.Verdict = "mismatch"
	}
	return result
}

// maxVerifyAttempts 单次鉴伪最多尝试的挑战数（目标 1 条有效回答）
const maxVerifyAttempts = 3

// verifyWithRetries 发挑战直到拿到一条有效回答或用尽尝试次数
func (mts *ModelTraceService) verifyWithRetries(provider *Provider, platform, apiModel string, bank *modeltrace.Bank) (ModelTraceResult, int, string, error) {
	challenges := modeltrace.GenerateChallenges(maxVerifyAttempts)
	var lastRaw string
	var lastErr error
	attempts := 0
	for _, challenge := range challenges {
		attempts++
		text, err := mts.requestChallenge(provider, platform, apiModel, challenge.Prompt)
		if err != nil {
			lastErr = err
			continue
		}
		lastRaw = text
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

// completionAuthHeader 按认证方式返回 (header 名, header 值)
func completionAuthHeader(authType, apiKey string) (string, string) {
	switch strings.ToLower(strings.TrimSpace(authType)) {
	case "x-api-key":
		return "x-api-key", apiKey
	case "bearer", "", "custom":
		return "Authorization", "Bearer " + apiKey
	default:
		// 自定义 Header 名
		headerName := strings.TrimSpace(authType)
		if headerName == "" {
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

func buildAnthropicChallengeBody(model, prompt string) ([]byte, error) {
	content, _ := json.Marshal(prompt)
	body := map[string]interface{}{
		"model":      model,
		"max_tokens": 4096,
		"messages": []challengeMessages{
			{Role: "user", Content: content},
		},
	}
	return json.Marshal(body)
}

func buildOpenAIChallengeBody(model, prompt string) ([]byte, error) {
	content, _ := json.Marshal(prompt)
	body := map[string]interface{}{
		"model": model,
		"messages": []challengeMessages{
			{Role: "user", Content: content},
		},
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

// resolveChallengeEndpoint 解析挑战请求应使用的端点（与连通性测试同规则）
func resolveChallengeEndpoint(provider *Provider, platform string) string {
	if endpoint := strings.TrimSpace(provider.APIEndpoint); endpoint != "" {
		if !strings.HasPrefix(endpoint, "/") {
			endpoint = "/" + endpoint
		}
		return provider.GetEffectiveEndpoint("/v1/messages")
	}
	switch strings.ToLower(platform) {
	case ProviderKindClaude:
		if provider.GetUpstreamProtocol() == UpstreamProtocolOpenAIChat {
			return "/v1/responses"
		}
		return "/v1/messages"
	case ProviderKindCodex:
		return provider.ResolveOpenAIUpstreamEndpoint("/responses")
	default:
		if provider.GetUpstreamProtocol() == UpstreamProtocolOpenAIChat {
			return "/v1/chat/completions"
		}
		return "/v1/messages"
	}
}

// requestChallenge 向 provider 发送一条挑战并返回回答文本。
// 端点/协议解析逻辑与连通性测试一致；不传 temperature（用渠道默认，测线上真实状态）。
func (mts *ModelTraceService) requestChallenge(provider *Provider, platform, apiModel, prompt string) (string, error) {
	endpoint := resolveChallengeEndpoint(provider, platform)
	protocol := provider.ResolveUpstreamProtocol(endpoint)
	targetURL := joinURL(provider.APIURL, endpoint)

	var reqBody []byte
	var err error
	isAnthropic := protocol == UpstreamProtocolAnthropic
	if isAnthropic {
		reqBody, err = buildAnthropicChallengeBody(apiModel, prompt)
	} else {
		// OpenAI Chat Completions 格式（/responses 与 /chat/completions 均按 chat 格式请求，
		// 挑战场景下足够；若上游仅支持 /responses 会返回错误并提示）
		reqBody, err = buildOpenAIChallengeBody(apiModel, prompt)
	}
	if err != nil {
		return "", fmt.Errorf("构造请求失败: %w", err)
	}

	authType := strings.TrimSpace(provider.ConnectivityAuthType)
	if authType == "" {
		authType = "bearer"
	}

	ctx, cancel := context.WithTimeout(context.Background(), mts.client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, compactUpstreamError(string(body)))
	}
	return extractCompletionText(body, isAnthropic)
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
		// 部分上游把 /responses 形态的 output 数组原样返回
		var responsesPayload struct {
			Output []struct {
				Content []struct {
					Type        string          `json:"type"`
					Text        string          `json:"text"`
					OutputText  json.RawMessage `json:"output_text"`
				} `json:"content"`
			} `json:"output"`
		}
		if err := json.Unmarshal(body, &responsesPayload); err == nil {
			var text strings.Builder
			for _, output := range responsesPayload.Output {
				for _, part := range output.Content {
					if part.Type == "output_text" {
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
			}
			if text.Len() > 0 {
				return text.String(), nil
			}
		}
		return "", errors.New("上游响应中没有 choices")
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
