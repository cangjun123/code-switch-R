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

func TestExtractResponsesText(t *testing.T) {
	// 正常 Responses 形态
	body, _ := json.Marshal(map[string]interface{}{
		"status": "completed",
		"output": []map[string]interface{}{
			{
				"content": []map[string]string{
					{"type": "output_text", "text": "5 8 13"},
				},
			},
		},
	})
	text, err := extractResponsesText(body)
	if err != nil || text != "5 8 13" {
		t.Fatalf("text=%q err=%v", text, err)
	}

	// 截断（max_output_tokens）-> 丢弃
	body, _ = json.Marshal(map[string]interface{}{
		"status":            "incomplete",
		"incomplete_details": map[string]string{"reason": "max_output_tokens"},
		"output": []map[string]interface{}{
			{"content": []map[string]string{{"type": "output_text", "text": "5 8"}}},
		},
	})
	if _, err := extractResponsesText(body); err == nil {
		t.Fatal("incomplete 响应未被拒绝")
	}
}

func TestVerifyProviderModelMappingOutOfBank(t *testing.T) {
	// 模型映射目标不在指纹库内 -> 应报 error 而非 mismatch
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	ps := NewProviderService()
	if err := ps.SaveProviders("claude", []Provider{
		{
			ID:           7,
			Name:         "mapper",
			APIURL:       "https://example.com",
			ModelMapping: map[string]string{"gpt-5.4": "gpt-4o-mini"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewModelTraceService(ps)
	result := svc.VerifyProviderModel("claude", 7, "gpt-5.4")
	if result.Verdict != "error" {
		t.Errorf("映射目标不在库内应报 error, got %v (msg=%s)", result.Verdict, result.Message)
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
		{
			name:     "custom平台默认chat",
			provider: Provider{UpstreamProtocol: "anthropic"},
			platform: "custom:mytool",
			expected: "/v1/chat/completions",
		},
		{
			name:     "连通性测试端点优先",
			provider: Provider{ConnectivityTestEndpoint: "/v1/chat/completions"},
			platform: "claude",
			expected: "/v1/chat/completions",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveConnectivityEndpoint(&c.provider, c.platform)
			if got != c.expected {
				t.Errorf("endpoint = %s, 期望 %s", got, c.expected)
			}
		})
	}
}

func TestCompletionAuthHeader(t *testing.T) {
	cases := []struct {
		authType string
		name     string
		value    string
	}{
		{"bearer", "Authorization", "Bearer sk-1"},
		{"", "Authorization", "Bearer sk-1"},
		{"x-api-key", "x-api-key", "sk-1"},
		{"custom", "Authorization", "sk-1"}, // custom 语义与连通性测试一致：无 Bearer 前缀
		{"X-Custom-Key", "X-Custom-Key", "sk-1"},
	}
	for _, c := range cases {
		name, value := completionAuthHeader(c.authType, "sk-1")
		if name != c.name || value != c.value {
			t.Errorf("authType=%q -> (%q, %q), 期望 (%q, %q)", c.authType, name, value, c.name, c.value)
		}
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
	// 指纹库模型数随上游更新增长，这里校验非空且字段完整即可
	if len(models) == 0 {
		t.Fatal("指纹库模型列表为空")
	}
	for _, m := range models {
		if m.ID == "" || m.DisplayName == "" || m.Family == "" || m.FamilyName == "" {
			t.Fatalf("模型条目字段不完整: %+v", m)
		}
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
	if minimum < 80 {
		t.Errorf("minimum = %d", minimum)
	}
}

func TestExtractStreamDelta(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantText   string
		wantStop   string
		wantErr    bool
	}{
		{
			name:     "OpenAI Responses output_text.delta (string delta)",
			payload:  `{"type":"response.output_text.delta","item_id":"msg_1","delta":"42, 108, "}`,
			wantText: "42, 108, ",
		},
		{
			name:     "OpenAI Responses reasoning_summary_text.delta",
			payload:  `{"type":"response.reasoning_summary_text.delta","delta":"thinking about numbers"}`,
			wantText: "thinking about numbers",
		},
		{
			name:     "OpenAI Responses completed",
			payload:  `{"type":"response.completed","response":{"id":"resp_1"}}`,
			wantStop: "stop",
		},
		{
			name:     "OpenAI Responses incomplete",
			payload:  `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_tokens"}}}`,
			wantStop: "max_tokens",
		},
		{
			name:     "Anthropic content_block_delta text_delta",
			payload:  `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"123, 456"}}`,
			wantText: "123, 456",
		},
		{
			name:     "Anthropic content_block_delta thinking_delta",
			payload:  `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"generating..."}}`,
			wantText: "generating...",
		},
		{
			name:     "Anthropic message_delta with delta.stop_reason",
			payload:  `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
			wantStop: "end_turn",
		},
		{
			name:     "OpenAI Chat content chunk",
			payload:  `{"choices":[{"index":0,"delta":{"content":"789, "}}]}`,
			wantText: "789, ",
		},
		{
			name:     "OpenAI Chat reasoning_content chunk",
			payload:  `{"choices":[{"index":0,"delta":{"reasoning_content":"reasoning step"}}]}`,
			wantText: "reasoning step",
		},
		{
			name:     "OpenAI Chat finish_reason",
			payload:  `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			wantStop: "stop",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, stop, err := extractStreamDelta(tt.payload)
			if (err != nil) != tt.wantErr {
				t.Fatalf("extractStreamDelta() error = %v, wantErr %v", err, tt.wantErr)
			}
			if text != tt.wantText {
				t.Errorf("extractStreamDelta() text = %q, want %q", text, tt.wantText)
			}
			if stop != tt.wantStop {
				t.Errorf("extractStreamDelta() stop = %q, want %q", stop, tt.wantStop)
			}
		})
	}
}
