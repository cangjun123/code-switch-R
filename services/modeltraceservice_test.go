package services

import (
	"encoding/json"
	"testing"

	"codeswitch/services/modeltrace"
)

func TestExtractCompletionTextAnthropic(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"content": []map[string]string{
			{"type": "text", "text": "12 34 56"},
		},
		"stop_reason": "end_turn",
	})
	text, err := extractCompletionText(body, true)
	if err != nil || text != "12 34 56" {
		t.Fatalf("text=%q err=%v", text, err)
	}

	// max_tokens 截断 -> 丢弃
	body, _ = json.Marshal(map[string]interface{}{
		"content":     []map[string]string{{"type": "text", "text": "12"}},
		"stop_reason": "max_tokens",
	})
	if _, err := extractCompletionText(body, true); err == nil {
		t.Fatal("max_tokens 截断未被拒绝")
	}

	// 拒答 -> 丢弃
	body, _ = json.Marshal(map[string]interface{}{
		"content":     []map[string]string{{"type": "text", "text": "no"}},
		"stop_reason": "refusal",
	})
	if _, err := extractCompletionText(body, true); err == nil {
		t.Fatal("refusal 未被拒绝")
	}
}

func TestExtractCompletionTextOpenAI(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"choices": []map[string]interface{}{
			{
				"message":       map[string]interface{}{"content": "78 90"},
				"finish_reason": "stop",
			},
		},
	})
	text, err := extractCompletionText(body, false)
	if err != nil || text != "78 90" {
		t.Fatalf("text=%q err=%v", text, err)
	}

	// content 为分段数组
	body, _ = json.Marshal(map[string]interface{}{
		"choices": []map[string]interface{}{
			{
				"message": map[string]interface{}{
					"content": []map[string]string{{"type": "text", "text": "11 22"}, {"type": "text", "text": " 33"}},
				},
				"finish_reason": "stop",
			},
		},
	})
	text, err = extractCompletionText(body, false)
	if err != nil || text != "11 22 33" {
		t.Fatalf("segmented text=%q err=%v", text, err)
	}

	// length 截断 -> 丢弃
	body, _ = json.Marshal(map[string]interface{}{
		"choices": []map[string]interface{}{
			{
				"message":       map[string]interface{}{"content": "x"},
				"finish_reason": "length",
			},
		},
	})
	if _, err := extractCompletionText(body, false); err == nil {
		t.Fatal("length 截断未被拒绝")
	}
}

func TestResolveChallengeEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		provider Provider
		platform string
		expected string
	}{
		{
			name:     "claude默认anthropic",
			provider: Provider{UpstreamProtocol: "anthropic"},
			platform: "claude",
			expected: "/v1/messages",
		},
		{
			name:     "claude openai协议",
			provider: Provider{UpstreamProtocol: "openai_chat"},
			platform: "claude",
			expected: "/v1/responses",
		},
		{
			name:     "claude自定义端点",
			provider: Provider{APIEndpoint: "/v1/chat/completions"},
			platform: "claude",
			expected: "/v1/chat/completions",
		},
		{
			name:     "codex默认",
			provider: Provider{},
			platform: "codex",
			expected: "/responses",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveChallengeEndpoint(&c.provider, c.platform)
			if got != c.expected {
				t.Errorf("endpoint = %s, 期望 %s", got, c.expected)
			}
		})
	}
}

func TestModelTraceVerifyValidation(t *testing.T) {
	svc := NewModelTraceService(NewProviderService())

	// 空模型
	if r := svc.VerifyProviderModel("claude", 1, ""); r.Verdict != "error" {
		t.Errorf("空模型应报错, got %v", r.Verdict)
	}
	// 指纹库外模型
	if r := svc.VerifyProviderModel("claude", 1, "gpt-4o"); r.Verdict != "error" {
		t.Errorf("库外模型应报错, got %v", r.Verdict)
	}
	// 指纹库内模型 + 不存在的 provider
	if r := svc.VerifyProviderModel("claude", 99999, "gpt-5.4"); r.Verdict != "error" {
		t.Errorf("不存在的 provider 应报错, got %v", r.Verdict)
	}
}

func TestModelTraceSupportedModels(t *testing.T) {
	svc := NewModelTraceService(NewProviderService())
	models, err := svc.GetSupportedModels()
	if err != nil {
		t.Fatalf("GetSupportedModels 失败: %v", err)
	}
	if len(models) != 13 {
		t.Fatalf("模型数 = %d, 期望 13", len(models))
	}
}

// 确认 modeltrace 包的 challenge 生成满足解析阈值
func TestChallengeOutputSelfConsistency(t *testing.T) {
	challenge := modeltrace.GenerateChallenges(1)[0]
	if challenge.ExpectedCount < 292 {
		t.Fatalf("expected_count = %d", challenge.ExpectedCount)
	}
	minimum := challenge.ExpectedCount * 55 / 100
	if minimum < modeltrace.MinimumValidNumbers {
		minimum = modeltrace.MinimumValidNumbers
	}
	// 292 * 0.55 = 160.6 > 80，阈值由 expected_count 决定
	if minimum < 80 {
		t.Errorf("minimum = %d", minimum)
	}
}
