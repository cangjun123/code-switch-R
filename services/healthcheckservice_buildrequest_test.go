package services

import (
	"encoding/json"
	"testing"
)

// 解析 buildTestRequest 的返回体为 map，便于断言字段
func parseProbeBody(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("解析探测请求体失败: %v, body=%s", err, string(data))
	}
	return m
}

func intField(m map[string]any, key string) int {
	if v, ok := m[key]; ok {
		if f, ok := v.(float64); ok {
			return int(f)
		}
	}
	return 0
}

// TestBuildTestRequestDefaultMaxTokens 验证默认（未配置 max_tokens）时文本类请求 max_tokens=1
func TestBuildTestRequestDefaultMaxTokens(t *testing.T) {
	hcs := &HealthCheckService{}
	provider := &Provider{ID: 1, Name: "p", APIURL: "https://x", APIKey: "k"}

	// claude /messages
	body := parseProbeBody(t, hcs.buildTestRequest(provider, "claude", "/v1/messages", "claude-haiku"))
	if intField(body, "max_tokens") != 1 {
		t.Fatalf("claude 默认 max_tokens 期望 1, 实际 %v", body["max_tokens"])
	}
	if _, ok := body["stream"]; ok {
		t.Fatalf("默认不应带 stream 字段")
	}

	// openai chat
	body = parseProbeBody(t, hcs.buildTestRequest(provider, "codex", "/v1/chat/completions", "gpt-4o-mini"))
	if intField(body, "max_tokens") != 1 {
		t.Fatalf("chat 默认 max_tokens 期望 1, 实际 %v", body["max_tokens"])
	}
}

// TestBuildTestRequestCustomMaxTokens 验证配置 max_tokens 后生效，且 stream 开关加上
func TestBuildTestRequestCustomMaxTokensAndStream(t *testing.T) {
	hcs := &HealthCheckService{}
	provider := &Provider{
		ID:                  1,
		Name:                "p",
		APIURL:              "https://x",
		APIKey:              "k",
		AvailabilityConfig:  &AvailabilityConfig{MaxTokens: 8, Stream: true},
	}

	body := parseProbeBody(t, hcs.buildTestRequest(provider, "claude", "/v1/messages", "claude-haiku"))
	if intField(body, "max_tokens") != 8 {
		t.Fatalf("claude 配置 max_tokens 期望 8, 实际 %v", body["max_tokens"])
	}
	if body["stream"] != true {
		t.Fatalf("stream 期望 true, 实际 %v", body["stream"])
	}

	// codex responses: max_output_tokens，且默认不丢弃时应为 8
	body = parseProbeBody(t, hcs.buildTestRequest(provider, "codex", "/responses", "gpt-4o-mini"))
	if intField(body, "max_output_tokens") != 8 {
		t.Fatalf("responses 配置 max_output_tokens 期望 8, 实际 %v", body["max_output_tokens"])
	}
	if body["stream"] != true {
		t.Fatalf("responses stream 期望 true, 实际 %v", body["stream"])
	}
}

// TestBuildTestRequestImageIgnoresMaxTokens 验证 gpt-image 不下发 max_tokens/stream
func TestBuildTestRequestImageIgnoresMaxTokensAndStream(t *testing.T) {
	hcs := &HealthCheckService{}
	provider := &Provider{
		ID:                 1,
		Name:               "p",
		APIURL:             "https://x",
		APIKey:             "k",
		AvailabilityConfig: &AvailabilityConfig{MaxTokens: 99, Stream: true},
	}

	body := parseProbeBody(t, hcs.buildTestRequest(provider, "gpt-image", "/v1/images/generations", "gpt-image-1"))
	if _, ok := body["max_tokens"]; ok {
		t.Fatalf("gpt-image 不应包含 max_tokens, body=%v", body)
	}
	if _, ok := body["max_output_tokens"]; ok {
		t.Fatalf("gpt-image 不应包含 max_output_tokens, body=%v", body)
	}
	if _, ok := body["stream"]; ok {
		t.Fatalf("gpt-image 不应包含 stream, body=%v", body)
	}
	if body["prompt"] != "health check" {
		t.Fatalf("gpt-image prompt 期望 health check, 实际 %v", body["prompt"])
	}
}

// TestClampPollIntervalSeconds 验证检测间隔范围保护
func TestClampPollIntervalSeconds(t *testing.T) {
	tests := []struct {
		name    string
		input   int
		want    int
	}{
		{"过小回退默认", 1, DefaultPollIntervalSeconds},
		{"刚好下限", MinPollIntervalSeconds, MinPollIntervalSeconds},
		{"正常值", 120, 120},
		{"刚好上限", MaxPollIntervalSeconds, MaxPollIntervalSeconds},
		{"过大回退默认", 99999, DefaultPollIntervalSeconds},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clampPollIntervalSeconds(tt.input)
			if got != tt.want {
				t.Fatalf("clampPollIntervalSeconds(%d) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}
