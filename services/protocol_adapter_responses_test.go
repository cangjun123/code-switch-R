package services

import (
	"errors"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertAnthropicToOpenAIResponses(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5.4",
		"system": [{"type":"text","text":"system rule"}],
		"messages": [
			{"role":"user","content":[{"type":"text","text":"hello"}]},
			{"role":"assistant","content":[
				{"type":"text","text":"working"},
				{"type":"tool_use","id":"call_1","name":"lookup","input":{"city":"Shanghai"}}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"done"}]}
		],
		"max_tokens": 128,
		"temperature": 0.2,
		"top_p": 0.8,
		"stream": true,
		"tools": [{"name":"lookup","description":"Lookup weather","input_schema":{"type":"object"}}],
		"tool_choice": {"type":"any"},
		"thinking": {"type":"enabled","budget_tokens": 6000},
		"output_format": {"type":"json_schema","schema":{"type":"object"}},
		"context_management": {
			"edits": [{"type":"compact_20260112","trigger":{"type":"input_tokens","value":150000}}]
		},
		"metadata": {"user_id":"user-1234567890","trace_id":"trace-1"},
		"stop_sequences": ["END"]
	}`)

	converted, info, err := ConvertAnthropicToOpenAIResponses(body)
	if err != nil {
		t.Fatalf("ConvertAnthropicToOpenAIResponses 返回错误: %v", err)
	}

	result := gjson.ParseBytes(converted)

	if got := result.Get("model").String(); got != "gpt-5.4" {
		t.Fatalf("model = %q, want %q", got, "gpt-5.4")
	}
	if got := result.Get("instructions").String(); got != "system rule" {
		t.Fatalf("instructions = %q, want %q", got, "system rule")
	}
	if got := result.Get("max_output_tokens").Int(); got != 128 {
		t.Fatalf("max_output_tokens = %d, want 128", got)
	}
	if !result.Get("stream").Bool() {
		t.Fatalf("stream should be true")
	}
	if got := result.Get("input.0.type").String(); got != "message" {
		t.Fatalf("input[0].type = %q, want message", got)
	}
	if got := result.Get("input.1.type").String(); got != "message" {
		t.Fatalf("input[1].type = %q, want message", got)
	}
	if got := result.Get("input.2.type").String(); got != "function_call" {
		t.Fatalf("input[2].type = %q, want function_call", got)
	}
	if got := result.Get("input.3.type").String(); got != "function_call_output" {
		t.Fatalf("input[3].type = %q, want function_call_output", got)
	}
	if got := result.Get("input.2.arguments").String(); got != `{"city":"Shanghai"}` {
		t.Fatalf("function_call arguments = %q", got)
	}
	if got := result.Get("tools.0.type").String(); got != "function" {
		t.Fatalf("tools[0].type = %q, want function", got)
	}
	if got := result.Get("tool_choice.type").String(); got != "required" {
		t.Fatalf("tool_choice.type = %q, want required", got)
	}
	if got := result.Get("reasoning.effort").String(); got != "medium" {
		t.Fatalf("reasoning.effort = %q, want medium", got)
	}
	if got := result.Get("text.format.type").String(); got != "json_schema" {
		t.Fatalf("text.format.type = %q, want json_schema", got)
	}
	if got := result.Get("context_management.0.type").String(); got != "compaction" {
		t.Fatalf("context_management[0].type = %q, want compaction", got)
	}
	if result.Get("user").Exists() {
		t.Fatalf("user should be dropped for compatibility, got %q", result.Get("user").String())
	}
	if len(info.DroppedMetadataKeys) != 2 || info.DroppedMetadataKeys[0] != "trace_id" || info.DroppedMetadataKeys[1] != "user_id" {
		t.Fatalf("DroppedMetadataKeys = %v, want [trace_id user_id]", info.DroppedMetadataKeys)
	}
	if len(info.DroppedFields) != 1 || info.DroppedFields[0] != "stop_sequences" {
		t.Fatalf("DroppedFields = %v, want [stop_sequences]", info.DroppedFields)
	}
}

func TestConvertAnthropicToOpenAIResponsesDropsEmptyContextManagement(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5.4",
		"messages": [
			{"role":"user","content":[{"type":"text","text":"hello"}]}
		],
		"context_management": []
	}`)

	converted, _, err := ConvertAnthropicToOpenAIResponses(body)
	if err != nil {
		t.Fatalf("ConvertAnthropicToOpenAIResponses 返回错误: %v", err)
	}

	result := gjson.ParseBytes(converted)
	if result.Get("context_management").Exists() {
		t.Fatalf("context_management should be dropped for empty array input, got %s", result.Get("context_management").Raw)
	}
}

func TestConvertAnthropicToOpenAIResponsesDropsUnsupportedContextManagementEdits(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5.4",
		"messages": [
			{"role":"user","content":[{"type":"text","text":"hello"}]}
		],
		"context_management": {
			"edits": [{"type":"clear_20260112"}]
		}
	}`)

	converted, _, err := ConvertAnthropicToOpenAIResponses(body)
	if err != nil {
		t.Fatalf("ConvertAnthropicToOpenAIResponses 返回错误: %v", err)
	}

	result := gjson.ParseBytes(converted)
	if result.Get("context_management").Exists() {
		t.Fatalf("context_management should be dropped when no edits can be translated, got %s", result.Get("context_management").Raw)
	}
}

func TestConvertAnthropicToOpenAIResponsesRejectsWebSearchByDefault(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5.4",
		"messages": [
			{"role":"user","content":[{"type":"text","text":"search it"}]}
		],
		"tools": [{"type":"web_search_20250305","name":"web_search"}]
	}`)

	_, _, err := ConvertAnthropicToOpenAIResponses(body)
	if !errors.Is(err, ErrClientRequestRejected) {
		t.Fatalf("expected ErrClientRequestRejected, got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "supportsWebSearch=true") {
		t.Fatalf("expected explicit supportsWebSearch hint, got %v", err)
	}
}

func TestConvertAnthropicToOpenAIResponsesAllowsWebSearchWhenEnabled(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5.4",
		"messages": [
			{"role":"user","content":[{"type":"text","text":"search it"}]}
		],
		"tools": [{"type":"web_search_20250305","name":"web_search"}]
	}`)

	converted, _, err := ConvertAnthropicToOpenAIResponses(body, ResponsesConvertOptions{
		AllowWebSearch: true,
		ProviderName:   "test-openai",
	})
	if err != nil {
		t.Fatalf("ConvertAnthropicToOpenAIResponses returned error: %v", err)
	}

	result := gjson.ParseBytes(converted)
	if got := result.Get("tools.0.type").String(); got != "web_search_preview" {
		t.Fatalf("tools[0].type = %q, want web_search_preview", got)
	}
}

func TestConvertOpenAIResponsesToAnthropic(t *testing.T) {
	body := []byte(`{
		"id": "resp_1",
		"model": "gpt-5.4",
		"status": "completed",
		"output": [
			{"type":"reasoning","summary":[{"text":"plan"}]},
			{"type":"message","content":[{"type":"output_text","text":"hello"}]},
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"city\":\"Shanghai\"}"}
		],
		"usage": {
			"input_tokens": 10,
			"output_tokens": 5,
			"input_tokens_details": {"cached_tokens": 3},
			"output_tokens_details": {"reasoning_tokens": 2}
		}
	}`)

	converted, err := ConvertOpenAIResponsesToAnthropic(body)
	if err != nil {
		t.Fatalf("ConvertOpenAIResponsesToAnthropic 返回错误: %v", err)
	}

	result := gjson.ParseBytes(converted)

	if got := result.Get("type").String(); got != "message" {
		t.Fatalf("type = %q, want message", got)
	}
	if got := result.Get("model").String(); got != "gpt-5.4" {
		t.Fatalf("model = %q, want gpt-5.4", got)
	}
	if got := result.Get("content.0.type").String(); got != "thinking" {
		t.Fatalf("content[0].type = %q, want thinking", got)
	}
	if got := result.Get("content.1.type").String(); got != "text" {
		t.Fatalf("content[1].type = %q, want text", got)
	}
	if got := result.Get("content.2.type").String(); got != "tool_use" {
		t.Fatalf("content[2].type = %q, want tool_use", got)
	}
	if got := result.Get("content.2.input.city").String(); got != "Shanghai" {
		t.Fatalf("tool_use.input.city = %q, want Shanghai", got)
	}
	if got := result.Get("stop_reason").String(); got != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", got)
	}
	if got := result.Get("usage.cache_read_input_tokens").Int(); got != 3 {
		t.Fatalf("usage.cache_read_input_tokens = %d, want 3", got)
	}
	if got := result.Get("usage.output_tokens_details.reasoning_tokens").Int(); got != 2 {
		t.Fatalf("usage.output_tokens_details.reasoning_tokens = %d, want 2", got)
	}
}

func TestResponsesToAnthropicSSEConverter(t *testing.T) {
	converter := NewResponsesToAnthropicSSEConverter("gpt-5.4")

	lines := []string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4"}}`,
		`data: {"type":"response.output_item.added","item":{"id":"msg_1","type":"message"}}`,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"Hello"}`,
		`data: {"type":"response.output_item.done","item":{"id":"msg_1","type":"message"}}`,
		`data: {"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup"}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"city\":\"Shanghai\"}"}`,
		`data: {"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call"}}`,
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message"},{"type":"function_call"}],"usage":{"input_tokens":11,"output_tokens":7,"input_tokens_details":{"cached_tokens":2}}}}`,
	}

	var output strings.Builder
	for _, line := range lines {
		output.WriteString(converter.ProcessLine(line))
	}

	combined := output.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		`"type":"text_delta"`,
		`"type":"input_json_delta"`,
		`"stop_reason":"tool_use"`,
		`"cache_read_input_tokens":2`,
	} {
		if !strings.Contains(combined, want) {
			t.Fatalf("SSE 输出未包含 %q\n%s", want, combined)
		}
	}
}
