package services

import (
	"encoding/json"
	"strings"
)

// Gemini 原生 API 探测（可用性监控 / 连通性测试 / pelican 测试共用）

const (
	defaultGeminiProbeModel = "gemini-2.5-flash"
	// geminiProbeEndpointTemplate 中的 {model} 在发请求前替换为实际模型
	geminiProbeEndpointTemplate = "/v1beta/models/{model}:generateContent"
)

// geminiDefaultProbeModel 返回未配置测试模型时的默认模型（应用供应商的模型映射/白名单）
func geminiDefaultProbeModel(provider *Provider) string {
	if provider == nil {
		return defaultGeminiProbeModel
	}
	if mapped := provider.GetEffectiveModel(defaultGeminiProbeModel); mapped != "" && mapped != defaultGeminiProbeModel {
		return mapped
	}
	if provider.IsModelSupported(defaultGeminiProbeModel) {
		return defaultGeminiProbeModel
	}
	if fallback := firstConfiguredProviderModel(provider); fallback != "" {
		return fallback
	}
	return defaultGeminiProbeModel
}

// resolveGeminiProbeEndpoint 把端点模板中的 {model} 替换为实际模型；
// 未带模型占位的自定义端点原样返回。stream=true 时改用 SSE 流式端点。
func resolveGeminiProbeEndpoint(template, model string, stream bool) string {
	endpoint := strings.TrimSpace(template)
	if endpoint == "" {
		endpoint = geminiProbeEndpointTemplate
	}
	endpoint = strings.ReplaceAll(endpoint, "{model}", model)
	if stream && strings.HasSuffix(endpoint, ":generateContent") {
		endpoint = strings.TrimSuffix(endpoint, ":generateContent") + ":streamGenerateContent?alt=sse"
	}
	return endpoint
}

// defaultImageProbeEndpoint 生图供应商的默认可用性探测端点（GET 模型列表，不计费）
const defaultImageProbeEndpoint = "/v1/models"

// isModelListProbeEndpoint 端点是否为模型列表（以 GET 方式探测）
func isModelListProbeEndpoint(endpoint string) bool {
	path := strings.ToLower(strings.TrimSpace(endpoint))
	if idx := strings.Index(path, "?"); idx >= 0 {
		path = path[:idx]
	}
	return strings.HasSuffix(strings.TrimRight(path, "/"), "/models")
}

// isGeminiProbeEndpoint 判断端点是否为 Gemini 原生 generateContent 接口
func isGeminiProbeEndpoint(endpoint string) bool {
	lower := strings.ToLower(endpoint)
	return strings.Contains(lower, ":generatecontent") || strings.Contains(lower, ":streamgeneratecontent") ||
		strings.Contains(lower, "{model}")
}

// buildGeminiProbeRequest 构建最小成本的 generateContent 请求体
func buildGeminiProbeRequest(prompt string, maxOutputTokens int) []byte {
	body := map[string]any{
		"contents": []map[string]any{
			{"role": "user", "parts": []map[string]string{{"text": prompt}}},
		},
	}
	if maxOutputTokens > 0 {
		body["generationConfig"] = map[string]any{"maxOutputTokens": maxOutputTokens}
	}
	data, _ := json.Marshal(body)
	return data
}
