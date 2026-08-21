package services

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// mustJSONStringForTest 简易 JSON 字符串编码（测试数据不含控制字符）
func mustJSONStringForTest(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// ==================== ConvertAnthropicToOpenAI ====================

func TestConvertAnthropicToOpenAIFullConversation(t *testing.T) {
	body := []byte(`{
		"model": "glm-5.2",
		"system": [{"type":"text","text":"top-level system rule"}],
		"messages": [
			{"role":"user","content":[{"type":"text","text":"what is the weather"}]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"should call the tool"},
				{"type":"text","text":"let me look it up"},
				{"type":"tool_use","id":"call_1","name":"lookup","input":{"city":"Shanghai"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"sunny 25C"}]},
				{"type":"text","text":"thanks"}
			]},
			{"role":"system","content":[{"type":"text","text":"inline system rule"}]}
		],
		"max_tokens": 128,
		"stream": true,
		"tools": [{"name":"lookup","description":"Lookup weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],
		"tool_choice": {"type":"any"},
		"stop_sequences": ["END"]
	}`)

	converted, _, err := ConvertAnthropicToOpenAI(body, DefaultConvertOptions())
	if err != nil {
		t.Fatalf("ConvertAnthropicToOpenAI 返回错误: %v", err)
	}

	result := gjson.ParseBytes(converted)

	if got := result.Get("model").String(); got != "glm-5.2" {
		t.Fatalf("model = %q", got)
	}
	if !result.Get("stream").Bool() {
		t.Fatal("stream should be true")
	}
	if !result.Get("stream_options.include_usage").Bool() {
		t.Fatal("stream_options.include_usage should be injected")
	}
	if got := result.Get("max_tokens").Int(); got != 128 {
		t.Fatalf("max_tokens = %d", got)
	}
	if got := result.Get("stop.0").String(); got != "END" {
		t.Fatalf("stop[0] = %q", got)
	}

	// system → 首条 system 消息
	if got := result.Get("messages.0.role").String(); got != "system" {
		t.Fatalf("messages[0].role = %q, want system", got)
	}
	if got := result.Get("messages.0.content").String(); got != "top-level system rule" {
		t.Fatalf("messages[0].content = %q", got)
	}

	// user text
	if got := result.Get("messages.1.role").String(); got != "user" {
		t.Fatalf("messages[1].role = %q, want user", got)
	}
	if got := result.Get("messages.1.content").String(); got != "what is the weather" {
		t.Fatalf("messages[1].content = %q", got)
	}

	// assistant：text + tool_use（thinking 丢弃）
	if got := result.Get("messages.2.role").String(); got != "assistant" {
		t.Fatalf("messages[2].role = %q, want assistant", got)
	}
	if got := result.Get("messages.2.content").String(); got != "let me look it up" {
		t.Fatalf("messages[2].content = %q（thinking 应丢弃）", got)
	}
	if got := result.Get("messages.2.tool_calls.0.id").String(); got != "call_1" {
		t.Fatalf("tool_calls[0].id = %q", got)
	}
	if got := result.Get("messages.2.tool_calls.0.type").String(); got != "function" {
		t.Fatalf("tool_calls[0].type = %q", got)
	}
	if got := result.Get("messages.2.tool_calls.0.function.name").String(); got != "lookup" {
		t.Fatalf("tool_calls[0].function.name = %q", got)
	}
	if got := result.Get("messages.2.tool_calls.0.function.arguments").String(); got != `{"city":"Shanghai"}` {
		t.Fatalf("tool_calls[0].function.arguments = %q", got)
	}

	// user tool_result → 独立 role=tool 消息（在 user 消息之前），随后的 user text 合并
	if got := result.Get("messages.3.role").String(); got != "tool" {
		t.Fatalf("messages[3].role = %q, want tool", got)
	}
	if got := result.Get("messages.3.tool_call_id").String(); got != "call_1" {
		t.Fatalf("messages[3].tool_call_id = %q", got)
	}
	if got := result.Get("messages.3.content").String(); got != "sunny 25C" {
		t.Fatalf("messages[3].content = %q", got)
	}
	if got := result.Get("messages.4.role").String(); got != "user" {
		t.Fatalf("messages[4].role = %q, want user", got)
	}
	if got := result.Get("messages.4.content").String(); got != "thanks" {
		t.Fatalf("messages[4].content = %q", got)
	}

	// 内联 system（opencode 风格）
	if got := result.Get("messages.5.role").String(); got != "system" {
		t.Fatalf("messages[5].role = %q, want system", got)
	}
	if got := result.Get("messages.5.content").String(); got != "inline system rule" {
		t.Fatalf("messages[5].content = %q", got)
	}

	// tools
	if got := result.Get("tools.0.type").String(); got != "function" {
		t.Fatalf("tools[0].type = %q", got)
	}
	if got := result.Get("tools.0.function.name").String(); got != "lookup" {
		t.Fatalf("tools[0].function.name = %q", got)
	}
	if got := result.Get("tools.0.function.description").String(); got != "Lookup weather" {
		t.Fatalf("tools[0].function.description = %q", got)
	}
	if got := result.Get("tools.0.function.parameters.type").String(); got != "object" {
		t.Fatalf("tools[0].function.parameters.type = %q", got)
	}
	if !result.Get("tools.0.function.parameters.required.0").Exists() {
		t.Fatal("input_schema 应原样透传到 parameters")
	}

	// tool_choice any → required
	if got := result.Get("tool_choice").String(); got != "required" {
		t.Fatalf("tool_choice = %q, want required", got)
	}
}

func TestConvertAnthropicToOpenAIImageContent(t *testing.T) {
	body := []byte(`{
		"model": "glm-5.2",
		"stream": true,
		"messages": [
			{"role":"user","content":[
				{"type":"text","text":"describe this"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}
			]}
		]
	}`)

	converted, _, err := ConvertAnthropicToOpenAI(body, DefaultConvertOptions())
	if err != nil {
		t.Fatalf("ConvertAnthropicToOpenAI 返回错误: %v", err)
	}

	result := gjson.ParseBytes(converted)
	if got := result.Get("messages.0.content.0.type").String(); got != "text" {
		t.Fatalf("content[0].type = %q, want text", got)
	}
	if got := result.Get("messages.0.content.1.type").String(); got != "image_url" {
		t.Fatalf("content[1].type = %q, want image_url", got)
	}
	if got := result.Get("messages.0.content.1.image_url.url").String(); got != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("image_url = %q", got)
	}
}

func TestConvertAnthropicToOpenAIToolChoiceVariants(t *testing.T) {
	cases := []struct {
		name       string
		toolChoice string
		want       string
	}{
		{"auto", `{"type":"auto"}`, "auto"},
		{"any", `{"type":"any"}`, "required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}],"tool_choice":` + c.toolChoice + `}`)
			converted, _, err := ConvertAnthropicToOpenAI(body, DefaultConvertOptions())
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got := gjson.ParseBytes(converted).Get("tool_choice").String(); got != c.want {
				t.Fatalf("tool_choice = %q, want %q", got, c.want)
			}
		})
	}

	// tool_choice 指定工具名
	body := []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"tool","name":"lookup"}}`)
	converted, _, err := ConvertAnthropicToOpenAI(body, DefaultConvertOptions())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := gjson.ParseBytes(converted).Get("tool_choice.type").String(); got != "function" {
		t.Fatalf("tool_choice.type = %q", got)
	}
	if got := gjson.ParseBytes(converted).Get("tool_choice.function.name").String(); got != "lookup" {
		t.Fatalf("tool_choice.function.name = %q", got)
	}
}

// 服务端工具块（web_search 等开启过的会话历史）：
// assistant 的 server_tool_use 丢弃，user 的 web_search_tool_result 转文本保留上下文
func TestConvertAnthropicToOpenAIServerToolBlocks(t *testing.T) {
	body := []byte(`{
		"model": "glm-5.2",
		"stream": true,
		"messages": [
			{"role":"user","content":"search for latest go release"},
			{"role":"assistant","content":[
				{"type":"text","text":"searching"},
				{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"go release"}}
			]},
			{"role":"user","content":[
				{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result_block","title":"Go 1.24 released","url":"https://go.dev"}]},
				{"type":"text","text":"summarize"}
			]}
		]
	}`)

	converted, _, err := ConvertAnthropicToOpenAI(body, DefaultConvertOptions())
	if err != nil {
		t.Fatalf("server_tool_use 会话应可转换: %v", err)
	}

	result := gjson.ParseBytes(converted)
	// assistant 消息：text 保留，server_tool_use 丢弃（无 tool_calls）
	if got := result.Get("messages.1.content").String(); got != "searching" {
		t.Fatalf("assistant content = %q", got)
	}
	if result.Get("messages.1.tool_calls").Exists() {
		t.Fatalf("server_tool_use 不应映射为 tool_calls")
	}
	// user 消息：搜索结果转文本 + 原 text
	userContent := result.Get("messages.2.content").String()
	if !strings.Contains(userContent, "Go 1.24 released") {
		t.Fatalf("web_search_tool_result 应转文本保留, got %q", userContent)
	}
	if !strings.Contains(userContent, "summarize") {
		t.Fatalf("原 text 应保留, got %q", userContent)
	}
}

// 未知块类型降级：历史中任何块（含 assistant 侧 tool_result 等非常规形态、
// 未来新增的块类型）都转文本保上下文，绝不让整个会话被 400 拒绝。
func TestConvertAnthropicToOpenAIUnknownBlocksDegradeToText(t *testing.T) {
	body := []byte(`{
		"model": "glm-5.2",
		"stream": true,
		"messages": [
			{"role":"assistant","content":[
				{"type":"text","text":"before"},
				{"type":"tool_result","tool_use_id":"orphan_1","content":"orphan tool result from compressed history"},
				{"type":"future_block_type","text":"some future block"}
			]},
			{"role":"user","content":[
				{"type":"another_unknown_block","data":{"foo":"bar"}}
			]}
		]
	}`)

	converted, _, err := ConvertAnthropicToOpenAI(body, DefaultConvertOptions())
	if err != nil {
		t.Fatalf("未知块类型应降级而非拒绝: %v", err)
	}

	result := gjson.ParseBytes(converted)
	assistantContent := result.Get("messages.0.content").String()
	if !strings.Contains(assistantContent, "before") {
		t.Fatalf("原 text 应保留, got %q", assistantContent)
	}
	if !strings.Contains(assistantContent, "orphan tool result") {
		t.Fatalf("assistant 侧 tool_result 应降级为文本, got %q", assistantContent)
	}
	if !strings.Contains(assistantContent, "some future block") {
		t.Fatalf("未知块 text 字段应降级为文本, got %q", assistantContent)
	}
	userContent := result.Get("messages.1.content").String()
	if !strings.Contains(userContent, "foo") {
		t.Fatalf("user 侧未知块应 JSON 降级为文本, got %q", userContent)
	}
}

func TestConvertAnthropicToOpenAIStillRejectsNonStream(t *testing.T) {	body := []byte(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	_, _, err := ConvertAnthropicToOpenAI(body, DefaultConvertOptions())
	if !errors.Is(err, ErrClientRequestRejected) {
		t.Fatalf("stream=false 应被拒绝, err=%v", err)
	}
}

// ==================== OpenAIToAnthropicSSEConverter ====================

func runSSEConverter(t *testing.T, lines []string) string {
	t.Helper()
	converter := NewOpenAIToAnthropicSSEConverter("glm-5.2")
	var output strings.Builder
	for _, line := range lines {
		output.WriteString(converter.ProcessLine(line))
	}
	return output.String()
}

func TestOpenAIToAnthropicSSEConverter_FullToolCallFlow(t *testing.T) {
	output := runSSEConverter(t, []string{
		`data: {"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"thinking hard"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"answer text"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Shanghai\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"id":"chatcmpl-1","model":"glm-5.2","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":80},"completion_tokens_details":{"reasoning_tokens":30}}}`,
		`data: [DONE]`,
	})

	// message_start
	if !strings.Contains(output, "event: message_start\n") {
		t.Fatalf("缺少 message_start:\n%s", output)
	}
	// thinking delta
	if !strings.Contains(output, `"type":"thinking_delta"`) || !strings.Contains(output, `"thinking":"thinking hard"`) {
		t.Fatalf("缺少 thinking_delta:\n%s", output)
	}
	// text delta
	if !strings.Contains(output, `"type":"text_delta"`) || !strings.Contains(output, `"text":"answer text"`) {
		t.Fatalf("缺少 text_delta:\n%s", output)
	}
	// tool_use block start
	if !strings.Contains(output, `"type":"tool_use"`) || !strings.Contains(output, `"id":"call_1"`) || !strings.Contains(output, `"name":"lookup"`) {
		t.Fatalf("缺少 tool_use content_block_start:\n%s", output)
	}
	// input_json_delta 两段
	if strings.Count(output, `"type":"input_json_delta"`) != 2 {
		t.Fatalf("input_json_delta 应出现 2 次:\n%s", output)
	}
	if !strings.Contains(output, `"partial_json":"{\"city\":"`) || !strings.Contains(output, `"partial_json":"\"Shanghai\"}"`) {
		t.Fatalf("input_json_delta 内容错误:\n%s", output)
	}
	// tool call 前应关闭 text block
	textStopIdx := strings.Index(output, `"content_block_stop"`)
	toolStartIdx := strings.Index(output, `"tool_use"`)
	if textStopIdx < 0 || toolStartIdx < 0 || textStopIdx > toolStartIdx {
		t.Fatalf("text block 应在 tool_use block 之前关闭:\n%s", output)
	}
	// message_delta: stop_reason=tool_use + usage
	if !strings.Contains(output, `"stop_reason":"tool_use"`) {
		t.Fatalf("缺少 stop_reason=tool_use:\n%s", output)
	}
	if !strings.Contains(output, `"input_tokens":100`) || !strings.Contains(output, `"output_tokens":50`) {
		t.Fatalf("缺少 usage:\n%s", output)
	}
	if !strings.Contains(output, `"cache_read_input_tokens":80`) {
		t.Fatalf("缺少 cache_read_input_tokens:\n%s", output)
	}
	if !strings.Contains(output, `"reasoning_tokens":30`) {
		t.Fatalf("缺少 reasoning_tokens:\n%s", output)
	}
	// message_stop
	if !strings.Contains(output, "event: message_stop\n") {
		t.Fatalf("缺少 message_stop:\n%s", output)
	}
}

func TestOpenAIToAnthropicSSEConverter_PlainTextFlow(t *testing.T) {
	output := runSSEConverter(t, []string{
		`data: {"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"hello "},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"world"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	})

	if !strings.Contains(output, `"type":"text_delta"`) {
		t.Fatalf("缺少 text_delta:\n%s", output)
	}
	if strings.Count(output, `"type":"text_delta"`) != 2 {
		t.Fatalf("两个 text delta 应合并到同一 block:\n%s", output)
	}
	if !strings.Contains(output, `"stop_reason":"end_turn"`) {
		t.Fatalf("缺少 stop_reason=end_turn:\n%s", output)
	}
	if strings.Contains(output, `"type":"tool_use"`) {
		t.Fatalf("纯文本流不应出现 tool_use:\n%s", output)
	}
}

func TestOpenAIToAnthropicSSEConverter_MultipleToolCalls(t *testing.T) {
	output := runSSEConverter(t, []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"a","arguments":"{}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"b","arguments":"{}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	})

	if !strings.Contains(output, `"id":"call_1"`) || !strings.Contains(output, `"id":"call_2"`) {
		t.Fatalf("两个 tool call 都应有 content_block_start:\n%s", output)
	}
	if !strings.Contains(output, `"name":"a"`) || !strings.Contains(output, `"name":"b"`) {
		t.Fatalf("两个 tool name:\n%s", output)
	}
	// 两个 block 都应被关闭
	if strings.Count(output, "event: content_block_stop") != 2 {
		t.Fatalf("应有两个 content_block_stop:\n%s", output)
	}
}

// GLM/Kimi/DeepSeek 混合推理流：reasoning_content 与 content 逐 chunk 交错。
// Anthropic 语义中 thinking 只在正文之前：正文开始后迟到的 reasoning 丢弃，
// 整条流至多一个 thinking 块 + 一个 text 块（逐词碎块会导致客户端渲染异常）。
func TestOpenAIToAnthropicSSEConverter_InterleavedReasoningSingleBlocks(t *testing.T) {
	seq := []struct {
		kind string
		text string
	}{
		{"reasoning_content", "let me think"},
		{"content", "Hello!"},
		{"reasoning_content", " more thought"},
		{"content", " 👋"},
		{"reasoning_content", " and more"},
		{"content", " I'm ready"},
	}
	lines := []string{}
	for _, s := range seq {
		lines = append(lines, fmt.Sprintf(
			`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"%s":%s},"finish_reason":null}]}`,
			s.kind, mustJSONStringForTest(s.text)))
	}
	lines = append(lines,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	)

	output := runSSEConverter(t, lines)

	// 至多 2 个块：1 thinking + 1 text
	if got := strings.Count(output, `"type":"content_block_start"`); got != 2 {
		t.Fatalf("交错流应收敛为 2 个块（thinking+text），got %d:\n%s", got, output)
	}
	if got := strings.Count(output, "event: content_block_stop"); got != 2 {
		t.Fatalf("应有 2 个 content_block_stop, got %d:\n%s", got, output)
	}
	// 正文开始后的 reasoning 丢弃：thinking delta 只应有一次
	if got := strings.Count(output, `"type":"thinking_delta"`); got != 1 {
		t.Fatalf("thinking_delta 应只有 1 次（迟到 reasoning 丢弃），got %d:\n%s", got, output)
	}
	// text delta 3 次合并到同一块（index 相同）
	if got := strings.Count(output, `"type":"text_delta"`); got != 3 {
		t.Fatalf("text_delta 应有 3 次，got %d:\n%s", got, output)
	}
	// thinking 块在 text 开始时关闭（顺序：thinking stop 在 text start 之前）
	thinkStop := strings.Index(output, `"thinking"`)
	textStart := strings.Index(output, `"type":"text"`)
	if thinkStop < 0 || textStart < 0 {
		t.Fatalf("应有 thinking 与 text 块:\n%s", output)
	}
}

func TestOpenAIToAnthropicSSEConverter_IgnoresEventLinesAndPostStop(t *testing.T) {	converter := NewOpenAIToAnthropicSSEConverter("m")
	var output strings.Builder
	output.WriteString(converter.ProcessLine(`event: ping`))
	output.WriteString(converter.ProcessLine("")) // 空行
	output.WriteString(converter.ProcessLine(`data: {"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`))
	output.WriteString(converter.ProcessLine(`data: [DONE]`))
	// message_stop 之后的数据应被忽略
	postStop := converter.ProcessLine(`data: {"choices":[{"index":0,"delta":{"content":"late"},"finish_reason":null}]}`)

	accumulated := output.String()
	if strings.Contains(accumulated, "event: ping") {
		t.Fatal("event 行应被忽略")
	}
	if postStop != "" {
		t.Fatalf("message_stop 后应无输出, got %q", postStop)
	}
	if !strings.Contains(accumulated, `"type":"text_delta"`) {
		t.Fatal("正常 delta 应有输出")
	}
}
