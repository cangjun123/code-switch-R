package services

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// ========== 错误定义 ==========

// ErrClientRequestRejected 客户端请求被拒绝（不支持的格式/功能）
// 该错误会导致直接返回 400，不触发 provider 切换和拉黑
var ErrClientRequestRejected = errors.New("client request rejected")

// NewClientRequestRejectedError 创建带原因的客户端请求拒绝错误
func NewClientRequestRejectedError(reason string) error {
	return fmt.Errorf("%w: %s", ErrClientRequestRejected, reason)
}

// ========== 请求转换选项和结果 ==========

// ConvertOptions 请求转换选项
type ConvertOptions struct {
	IncludeUsage bool // 是否注入 stream_options.include_usage
}

// DefaultConvertOptions 默认转换选项
func DefaultConvertOptions() ConvertOptions {
	return ConvertOptions{
		IncludeUsage: true,
	}
}

// ConvertInfo 请求转换结果信息
type ConvertInfo struct {
	DroppedMetadataKeys []string // 被丢弃的 metadata 键
	MappedUser          string   // 映射到 OpenAI user 字段的值
	InjectedStreamOpts  bool     // 是否注入了 stream_options
	DroppedFields       []string // 被丢弃的顶层字段
}

// ========== 请求转换：Anthropic → OpenAI ==========

// ConvertAnthropicToOpenAI 将 Anthropic Messages 请求转换为 OpenAI Chat Completions 请求
// 支持完整对话形态：tools/tool_choice/tool_use/tool_result/图片/thinking 重放。
// 限制：仅支持 stream=true（Claude Code 恒为流式）。
func ConvertAnthropicToOpenAI(body []byte, opts ConvertOptions) ([]byte, ConvertInfo, error) {
	info := ConvertInfo{}

	// 解析 Anthropic 请求
	parsed := gjson.ParseBytes(body)

	// ========== 前置校验 ==========

	// 检查 stream（仅支持流式：非流式需要整套 JSON 响应回转，当前不做）
	streamVal := parsed.Get("stream")
	if !streamVal.Exists() || !streamVal.Bool() {
		return nil, info, NewClientRequestRejectedError("仅支持 stream=true 的请求")
	}

	// ========== 构建 OpenAI 请求 ==========

	openAIReq := make(map[string]interface{})

	// model（直接使用，已经过 ModelMapping 处理）
	if model := parsed.Get("model").String(); model != "" {
		openAIReq["model"] = model
	}

	// max_tokens
	if maxTokens := parsed.Get("max_tokens"); maxTokens.Exists() {
		openAIReq["max_tokens"] = maxTokens.Int()
	}

	// stream
	openAIReq["stream"] = true

	// stream_options（用于获取 usage）
	if opts.IncludeUsage {
		openAIReq["stream_options"] = map[string]interface{}{
			"include_usage": true,
		}
		info.InjectedStreamOpts = true
	}

	// temperature
	if temp := parsed.Get("temperature"); temp.Exists() {
		openAIReq["temperature"] = temp.Float()
	}

	// top_p
	if topP := parsed.Get("top_p"); topP.Exists() {
		openAIReq["top_p"] = topP.Float()
	}

	// stop_sequences → stop
	if stopSeqs := parsed.Get("stop_sequences"); stopSeqs.Exists() && stopSeqs.IsArray() {
		stops := make([]string, 0)
		for _, s := range stopSeqs.Array() {
			stops = append(stops, s.String())
		}
		if len(stops) > 0 {
			openAIReq["stop"] = stops
		}
	}

	// 记录被丢弃的 metadata 键
	if metadata := parsed.Get("metadata"); metadata.Exists() && metadata.IsObject() {
		metadata.ForEach(func(key, value gjson.Result) bool {
			info.DroppedMetadataKeys = append(info.DroppedMetadataKeys, key.String())
			return true
		})
	}

	// 记录被丢弃的顶层字段
	droppedTopLevel := []string{"betas", "anthropic_version"}
	for _, field := range droppedTopLevel {
		if parsed.Get(field).Exists() {
			info.DroppedFields = append(info.DroppedFields, field)
		}
	}

	// ========== 转换 messages ==========

	messages := make([]map[string]interface{}, 0)

	// system → 转为第一条 system 消息
	if system := parsed.Get("system"); system.Exists() {
		systemText, err := extractTextContent(system)
		if err != nil {
			return nil, info, err
		}
		if systemText != "" {
			messages = append(messages, map[string]interface{}{
				"role":    "system",
				"content": systemText,
			})
		}
	}

	// messages[]（完整对话形态：text/image/tool_use/tool_result/thinking）
	if msgArray := parsed.Get("messages"); msgArray.Exists() && msgArray.IsArray() {
		for i, msg := range msgArray.Array() {
			converted, err := convertAnthropicMessageToOpenAI(i, msg)
			if err != nil {
				return nil, info, err
			}
			messages = append(messages, converted...)
		}
	}

	openAIReq["messages"] = messages

	// ========== 转换 tools / tool_choice ==========

	if tools := parsed.Get("tools"); tools.Exists() && tools.IsArray() && len(tools.Array()) > 0 {
		openAITools, err := translateAnthropicToolsToOpenAI(tools)
		if err != nil {
			return nil, info, err
		}
		if len(openAITools) > 0 {
			openAIReq["tools"] = openAITools
		}
	}

	if toolChoice := parsed.Get("tool_choice"); toolChoice.Exists() {
		if converted, ok := translateAnthropicToolChoiceToOpenAI(toolChoice); ok {
			openAIReq["tool_choice"] = converted
		}
	}

	// 序列化
	result, err := json.Marshal(openAIReq)
	if err != nil {
		return nil, info, fmt.Errorf("序列化 OpenAI 请求失败: %w", err)
	}

	return result, info, nil
}

// convertAnthropicMessageToOpenAI 转换单条 Anthropic 消息为一条或多条 OpenAI 消息。
// user 的 tool_result block 会拆成独立的 role=tool 消息（OpenAI 规范要求）。
func convertAnthropicMessageToOpenAI(messageIndex int, msg gjson.Result) ([]map[string]interface{}, error) {
	role := msg.Get("role").String()
	content := msg.Get("content")

	switch role {
	case "user", "assistant", "system", "developer":
	default:
		return nil, NewClientRequestRejectedError(
			fmt.Sprintf("messages[%d].role='%s' 不支持", messageIndex, role))
	}

	// string content 快捷路径
	if content.Type == gjson.String {
		return []map[string]interface{}{{
			"role":    openAIRoleForAnthropic(role),
			"content": content.String(),
		}}, nil
	}
	if !content.IsArray() {
		return nil, fmt.Errorf("messages[%d].content: 必须是 string 或 block 数组", messageIndex)
	}

	// assistant/system/developer：text 合并为 content，tool_use 合并为 tool_calls，thinking 丢弃
	if role == "assistant" || role == "system" || role == "developer" {
		var texts []string
		toolCalls := make([]interface{}, 0)
		for _, block := range content.Array() {
			switch block.Get("type").String() {
			case "text":
				texts = append(texts, block.Get("text").String())
			case "thinking":
				// Chat Completions 无 thinking 重放概念，丢弃（模型自身产物）
			case "tool_use":
				arguments, err := json.Marshal(block.Get("input").Value())
				if err != nil {
					return nil, fmt.Errorf("messages[%d].tool_use.input 无法序列化: %w", messageIndex, err)
				}
				callID := block.Get("id").String()
				if callID == "" {
					callID = block.Get("tool_use_id").String()
				}
				toolCalls = append(toolCalls, map[string]interface{}{
					"id":   callID,
					"type": "function",
					"function": map[string]interface{}{
						"name":      block.Get("name").String(),
						"arguments": string(arguments),
					},
				})
			default:
				return nil, NewClientRequestRejectedError(
					fmt.Sprintf("messages[%d].content type='%s' 不支持", messageIndex, block.Get("type").String()))
			}
		}
		openAIMsg := map[string]interface{}{
			"role": openAIRoleForAnthropic(role),
		}
		if len(texts) > 0 {
			openAIMsg["content"] = strings.Join(texts, "\n")
		} else {
			openAIMsg["content"] = nil
		}
		if len(toolCalls) > 0 {
			openAIMsg["tool_calls"] = toolCalls
		}
		return []map[string]interface{}{openAIMsg}, nil
	}

	// user：text/image 组装 content，tool_result 拆成独立 role=tool 消息
	result := make([]map[string]interface{}, 0, 2)
	var textParts []string
	var imageParts []interface{}
	hasImage := false

	for _, block := range content.Array() {
		switch block.Get("type").String() {
		case "text":
			textParts = append(textParts, block.Get("text").String())
		case "image":
			imageURL, err := translateAnthropicImageSourceToURL(block.Get("source").Value())
			if err != nil {
				return nil, fmt.Errorf("messages[%d].image: %w", messageIndex, err)
			}
			hasImage = true
			imageParts = append(imageParts, map[string]interface{}{
				"type":      "image_url",
				"image_url": map[string]interface{}{"url": imageURL},
			})
		case "tool_result":
			// tool_result 拆成独立 tool 消息，放在当前 user 消息之前
			result = append(result, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": block.Get("tool_use_id").String(),
				"content":      stringifyAnthropicToolResultContent(block.Get("content").Value()),
			})
		default:
			return nil, NewClientRequestRejectedError(
				fmt.Sprintf("messages[%d].content type='%s' 不支持", messageIndex, block.Get("type").String()))
		}
	}

	var userContent interface{}
	if hasImage {
		parts := make([]interface{}, 0, len(textParts)+len(imageParts))
		for _, text := range textParts {
			parts = append(parts, map[string]interface{}{"type": "text", "text": text})
		}
		parts = append(parts, imageParts...)
		userContent = parts
	} else {
		userContent = strings.Join(textParts, "\n")
	}

	result = append(result, map[string]interface{}{
		"role":    "user",
		"content": userContent,
	})
	return result, nil
}

// openAIRoleForAnthropic 映射 Anthropic role 到 OpenAI role
func openAIRoleForAnthropic(role string) string {
	if role == "developer" {
		return "system"
	}
	return role
}

// translateAnthropicToolsToOpenAI 转换 Anthropic tools 到 OpenAI function tools
func translateAnthropicToolsToOpenAI(tools gjson.Result) ([]interface{}, error) {
	result := make([]interface{}, 0, len(tools.Array()))
	for i, tool := range tools.Array() {
		if strings.HasPrefix(tool.Get("type").String(), "web_search") ||
			strings.HasPrefix(tool.Get("name").String(), "web_search") {
			return nil, NewClientRequestRejectedError(
				fmt.Sprintf("tools[%d]: Chat Completions 上游不支持 web_search", i))
		}
		name := tool.Get("name").String()
		if name == "" {
			return nil, NewClientRequestRejectedError(fmt.Sprintf("tools[%d].name 不能为空", i))
		}
		function := map[string]interface{}{
			"name": name,
		}
		if description := tool.Get("description").String(); description != "" {
			function["description"] = description
		}
		if schema := tool.Get("input_schema"); schema.Exists() && schema.IsObject() {
			function["parameters"] = schema.Value()
		}
		result = append(result, map[string]interface{}{
			"type":     "function",
			"function": function,
		})
	}
	return result, nil
}

// translateAnthropicToolChoiceToOpenAI 转换 Anthropic tool_choice 到 OpenAI tool_choice
// 返回 (converted, ok)：tool_choice 不存在/无效时 ok=false 跳过
func translateAnthropicToolChoiceToOpenAI(toolChoice gjson.Result) (interface{}, bool) {
	if toolChoice.Type == gjson.String {
		switch toolChoice.String() {
		case "auto":
			return "auto", true
		case "any":
			return "required", true
		default:
			// 其他非空字符串视为指定工具名
			if name := toolChoice.String(); name != "" {
				return map[string]interface{}{
					"type": "function",
					"function": map[string]interface{}{
						"name": name,
					},
				}, true
			}
		}
		return nil, false
	}

	if toolChoice.IsObject() {
		switch toolChoice.Get("type").String() {
		case "auto":
			return "auto", true
		case "any":
			return "required", true
		case "tool":
			return map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name": toolChoice.Get("name").String(),
				},
			}, true
		}
	}
	return nil, false
}

// extractTextContent 从 Anthropic content 字段提取纯文本
// content 可能是 string 或 [{type:"text",text:"..."},...] 数组
func extractTextContent(content gjson.Result) (string, error) {
	if !content.Exists() {
		return "", nil
	}

	// 字符串形式
	if content.Type == gjson.String {
		return content.String(), nil
	}

	// 数组形式：拼接全部 text block
	if content.IsArray() {
		var texts []string
		for _, block := range content.Array() {
			if block.Get("type").String() == "text" {
				texts = append(texts, block.Get("text").String())
			}
		}
		return strings.Join(texts, "\n"), nil
	}

	return "", NewClientRequestRejectedError("content 格式无效，必须是 string 或 block 数组")
}

// ========== SSE 转换状态机：OpenAI → Anthropic ==========

// OpenAIToAnthropicSSEConverter OpenAI Chat Completions SSE 到 Anthropic SSE 的转换器
// 设计为支持逐行输入（适配 xrequest 的 hook 行为）。
// 支持完整对话形态：文本 delta、混合推理流（reasoning_content/reasoning）、
// tool_calls 增量（含多 call 交错）、扩展 usage（缓存/推理 token）。
type OpenAIToAnthropicSSEConverter struct {
	messageID      string // Anthropic message ID
	model          string // 模型名（用于 message_start）
	sentStart      bool   // 是否已输出 message_start
	sentStop       bool   // 是否已输出 message_stop
	finishReason   string // 捕获的 finish_reason
	inputTokens    int64  // 捕获的 input tokens
	outputTokens   int64  // 捕获的 output tokens
	cacheReadTok   int64  // prompt_tokens_details.cached_tokens
	reasoningTok   int64  // completion_tokens_details.reasoning_tokens
	usageCaptured  bool   // 是否已捕获 usage
	nextBlockIndex int    // 下一个分配的 content block 序号
	openBlocks     map[int]struct{}
	blockTypes     map[int]string  // block index → text/thinking
	toolCallBlocks map[int]int     // OpenAI tool_call index → block index
}

// NewOpenAIToAnthropicSSEConverter 创建新的 SSE 转换器
func NewOpenAIToAnthropicSSEConverter(model string) *OpenAIToAnthropicSSEConverter {
	return &OpenAIToAnthropicSSEConverter{
		messageID:      "msg_" + uuid.New().String()[:24],
		model:          model,
		nextBlockIndex: 0,
		openBlocks:     make(map[int]struct{}),
		blockTypes:     make(map[int]string),
		toolCallBlocks: make(map[int]int),
	}
}

// ProcessLine 处理单行输入（xrequest 的 hook 是逐行回调）
// 返回转换后的 Anthropic SSE 事件（可能为空，表示无输出）
func (c *OpenAIToAnthropicSSEConverter) ProcessLine(line string) string {
	if c.sentStop {
		return "" // 已结束，忽略后续数据
	}

	line = strings.TrimSpace(line)

	// 跳过空行、event 行和非 data: 行
	if line == "" || strings.HasPrefix(line, "event:") || !strings.HasPrefix(line, "data:") {
		return ""
	}

	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))

	// 检查 [DONE]
	if data == "[DONE]" {
		return c.emitCompletion()
	}

	// 解析 JSON
	parsed := gjson.Parse(data)

	// 提取 model（如果有）
	if model := parsed.Get("model").String(); model != "" && c.model == "" {
		c.model = model
	}

	// 提取 usage（stream_options.include_usage 开启时，最终 chunk 的 choices 为空数组）
	if usage := parsed.Get("usage"); usage.Exists() && usage.IsObject() && !c.usageCaptured {
		if usage.Get("prompt_tokens").Exists() || usage.Get("completion_tokens").Exists() {
			c.inputTokens = usage.Get("prompt_tokens").Int()
			c.outputTokens = usage.Get("completion_tokens").Int()
			c.cacheReadTok = usage.Get("prompt_tokens_details.cached_tokens").Int()
			c.reasoningTok = usage.Get("completion_tokens_details.reasoning_tokens").Int()
			c.usageCaptured = true
		}
	}

	// 处理 choices[0]
	choice := parsed.Get("choices.0")
	if !choice.Exists() {
		return "" // 无 choices，跳过（纯 usage chunk）
	}

	// 提取 finish_reason
	if fr := choice.Get("finish_reason"); fr.Exists() && fr.String() != "" {
		c.finishReason = fr.String()
	}
	if fr := choice.Get("delta.finish_reason"); fr.Exists() && fr.String() != "" {
		c.finishReason = fr.String()
	}

	var output strings.Builder

	// reasoning 增量（GLM/Kimi/DeepSeek 混合推理流：reasoning_content 或 reasoning）
	if reasoning := firstNonEmptyGjson(
		choice.Get("delta.reasoning_content"),
		choice.Get("delta.reasoning"),
	); reasoning != "" {
		output.WriteString(c.emitTextLikeDelta("thinking", reasoning))
	}

	// content 增量
	contentDelta := choice.Get("delta.content").String()
	// 兼容：有些上游用 message.content
	if contentDelta == "" {
		contentDelta = choice.Get("message.content").String()
	}
	if contentDelta != "" {
		output.WriteString(c.emitTextLikeDelta("text", contentDelta))
	}

	// tool_calls 增量
	toolCalls := choice.Get("delta.tool_calls")
	if toolCalls.Exists() && toolCalls.IsArray() {
		for i, call := range toolCalls.Array() {
			output.WriteString(c.processToolCallDelta(i, call))
		}
	}

	return output.String()
}

// processToolCallDelta 处理单个 tool_call 增量。
// 首见某 index：关闭仍开着的文本块，开 tool_use block；
// 后续：function.arguments 增量转 input_json_delta。
func (c *OpenAIToAnthropicSSEConverter) processToolCallDelta(arrayIndex int, call gjson.Result) string {
	// index 缺失时用数组位置兜底（部分 OpenAI 兼容实现省略）
	callIndex := arrayIndex
	if call.Get("index").Exists() {
		callIndex = int(call.Get("index").Int())
	}

	function := call.Get("function")

	// 首见该 call：发 content_block_start
	blockIndex, seen := c.toolCallBlocks[callIndex]
	var output strings.Builder
	if !seen {
		// tool call 到来意味着文本阶段结束，关闭仍开着的文本块
		output.WriteString(c.closeOpenTextBlocks())
		blockIndex = c.nextBlockIndex
		c.nextBlockIndex++
		c.toolCallBlocks[callIndex] = blockIndex
		c.openBlocks[blockIndex] = struct{}{}

		output.WriteString(c.emitAnthropicSSE("content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": blockIndex,
			"content_block": map[string]interface{}{
				"type":  "tool_use",
				"id":    call.Get("id").String(),
				"name":  function.Get("name").String(),
				"input": map[string]interface{}{},
			},
		}))
	}
	// 罕见：function.name 在后续 chunk 才到——Anthropic content_block_start 后 name 不可改，丢弃

	// arguments 增量 → input_json_delta
	if args := function.Get("arguments").String(); args != "" {
		output.WriteString(c.emitAnthropicSSE("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]interface{}{
				"type":         "input_json_delta",
				"partial_json": args,
			},
		}))
	}

	return output.String()
}

// emitTextLikeDelta 输出文本类增量（text 或 thinking）。按需懒建对应 block。
func (c *OpenAIToAnthropicSSEConverter) emitTextLikeDelta(blockType, text string) string {
	// 已有同类型开着的 block：直接追加 delta
	for index := range c.openBlocks {
		if _, isTool := c.toolCallBlocks[index]; !isTool && c.blockTypes[index] == blockType {
			return c.emitAnthropicSSE("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]interface{}{
					"type":    blockType + "_delta",
					blockType: text,
				},
			})
		}
	}

	// 无同类型开块：新开一个
	var output strings.Builder
	output.WriteString(c.ensureMessageStart())

	blockIndex := c.nextBlockIndex
	c.nextBlockIndex++
	c.openBlocks[blockIndex] = struct{}{}
	c.blockTypes[blockIndex] = blockType

	contentBlock := map[string]interface{}{"type": blockType}
	if blockType == "thinking" {
		contentBlock["thinking"] = ""
	} else {
		contentBlock["text"] = ""
	}

	output.WriteString(c.emitAnthropicSSE("content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         blockIndex,
		"content_block": contentBlock,
	}))
	output.WriteString(c.emitAnthropicSSE("content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": blockIndex,
		"delta": map[string]interface{}{
			"type":    blockType + "_delta",
			blockType: text,
		},
	}))
	return output.String()
}

// closeOpenTextBlocks 关闭所有仍开着的非 tool_use block（tool call 到来时文本阶段结束）
func (c *OpenAIToAnthropicSSEConverter) closeOpenTextBlocks() string {
	var output strings.Builder
	indexes := make([]int, 0, len(c.openBlocks))
	for index := range c.openBlocks {
		if _, isTool := c.toolCallBlocks[index]; !isTool {
			indexes = append(indexes, index)
		}
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		output.WriteString(c.emitContentBlockStop(index))
	}
	return output.String()
}

// emitCompletion 输出结束事件序列（[DONE] 时调用）
func (c *OpenAIToAnthropicSSEConverter) emitCompletion() string {
	if c.sentStop {
		return ""
	}
	c.sentStop = true

	var output strings.Builder
	output.WriteString(c.ensureMessageStart())
	output.WriteString(c.emitOpenBlockStops())

	// message_delta（包含 stop_reason 和 usage）
	stopReason := c.mapFinishReason(c.finishReason)
	usage := map[string]interface{}{
		"input_tokens":  c.inputTokens,
		"output_tokens": c.outputTokens,
	}
	if c.cacheReadTok > 0 {
		usage["cache_read_input_tokens"] = c.cacheReadTok
	}
	if c.reasoningTok > 0 {
		usage["output_tokens_details"] = map[string]interface{}{
			"reasoning_tokens": c.reasoningTok,
		}
	}
	output.WriteString(c.emitAnthropicSSE("message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": usage,
	}))
	output.WriteString(c.emitAnthropicSSE("message_stop", map[string]interface{}{
		"type": "message_stop",
	}))
	return output.String()
}

// ensureMessageStart 惰性输出 message_start
func (c *OpenAIToAnthropicSSEConverter) ensureMessageStart() string {
	if c.sentStart {
		return ""
	}
	c.sentStart = true

	return c.emitAnthropicSSE("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            c.messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         c.model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	})
}

// emitOpenBlockStops 关闭全部仍开着的 block
func (c *OpenAIToAnthropicSSEConverter) emitOpenBlockStops() string {
	var output strings.Builder
	indexes := make([]int, 0, len(c.openBlocks))
	for index := range c.openBlocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		output.WriteString(c.emitContentBlockStop(index))
	}
	return output.String()
}

// emitContentBlockStop 关闭单个 block（若开着）
func (c *OpenAIToAnthropicSSEConverter) emitContentBlockStop(index int) string {
	if _, ok := c.openBlocks[index]; !ok {
		return ""
	}
	delete(c.openBlocks, index)

	return c.emitAnthropicSSE("content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": index,
	})
}

// emitAnthropicSSE 输出标准 Anthropic 双行 SSE 事件
func (c *OpenAIToAnthropicSSEConverter) emitAnthropicSSE(event string, payload interface{}) string {
	body, _ := json.Marshal(payload)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(body))
}

// mapFinishReason 映射 OpenAI finish_reason 到 Anthropic stop_reason
func (c *OpenAIToAnthropicSSEConverter) mapFinishReason(finishReason string) string {
	switch finishReason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "end_turn"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// GetUsage 获取捕获的 usage 信息
func (c *OpenAIToAnthropicSSEConverter) GetUsage() (inputTokens, outputTokens int64) {
	return c.inputTokens, c.outputTokens
}

// firstNonEmptyGjson 返回第一个非空字符串结果
func firstNonEmptyGjson(values ...gjson.Result) string {
	for _, value := range values {
		if value.Exists() && value.String() != "" {
			return value.String()
		}
	}
	return ""
}

// ========== 辅助函数 ==========

// GenerateAnthropicMessageID 生成 Anthropic 风格的 message ID
func GenerateAnthropicMessageID() string {
	return fmt.Sprintf("msg_%s", uuid.New().String()[:24])
}

// GenerateAnthropicTimestamp 生成 Anthropic 风格的时间戳
func GenerateAnthropicTimestamp() string {
	return time.Now().UTC().Format(time.RFC3339)
}
