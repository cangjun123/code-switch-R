package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

// SSEProtocolConverter 流式协议转换器接口
type SSEProtocolConverter interface {
	ProcessLine(line string) string
}

// ResponsesConvertOptions Responses API 转换选项
type ResponsesConvertOptions struct {
	AllowWebSearch bool
	ProviderName   string
}

// DefaultResponsesConvertOptions 默认 Responses API 转换选项
func DefaultResponsesConvertOptions() ResponsesConvertOptions {
	return ResponsesConvertOptions{}
}

// ConvertAnthropicToOpenAIResponses 将 Anthropic Messages 请求转换为 OpenAI Responses API 请求
func ConvertAnthropicToOpenAIResponses(body []byte, opts ...ResponsesConvertOptions) ([]byte, ConvertInfo, error) {
	info := ConvertInfo{}
	options := DefaultResponsesConvertOptions()
	if len(opts) > 0 {
		options = opts[0]
	}

	req, err := decodeJSONObject(body)
	if err != nil {
		return nil, info, NewClientRequestRejectedError("请求体不是合法 JSON")
	}

	responsesReq := make(map[string]interface{})

	if model := asString(req["model"]); model != "" {
		responsesReq["model"] = model
	}

	inputItems, err := translateAnthropicMessagesToResponsesInput(req["messages"])
	if err != nil {
		return nil, info, err
	}
	responsesReq["input"] = inputItems

	if instructions := translateAnthropicSystemToInstructions(req["system"]); instructions != "" {
		responsesReq["instructions"] = instructions
	}

	if maxTokens, ok := req["max_tokens"]; ok && maxTokens != nil {
		responsesReq["max_output_tokens"] = maxTokens
	}
	if temperature, ok := req["temperature"]; ok && temperature != nil {
		responsesReq["temperature"] = temperature
	}
	if topP, ok := req["top_p"]; ok && topP != nil {
		responsesReq["top_p"] = topP
	}
	if stream, ok := req["stream"].(bool); ok {
		responsesReq["stream"] = stream
	}

	if tools, err := translateAnthropicToolsToResponsesTools(req["tools"], options); err != nil {
		return nil, info, err
	} else if len(tools) > 0 {
		responsesReq["tools"] = tools
	}

	if toolChoice, err := translateAnthropicToolChoiceToResponses(req["tool_choice"]); err != nil {
		return nil, info, err
	} else if toolChoice != nil {
		responsesReq["tool_choice"] = toolChoice
	}

	if reasoning := translateAnthropicThinkingToResponses(req["thinking"]); reasoning != nil {
		responsesReq["reasoning"] = reasoning
	}

	if textConfig := translateAnthropicOutputFormatToResponses(req); textConfig != nil {
		responsesReq["text"] = textConfig
	}

	if contextManagement := translateAnthropicContextManagementToResponses(req["context_management"]); contextManagement != nil {
		responsesReq["context_management"] = contextManagement
	}

	if metadata, ok := req["metadata"].(map[string]interface{}); ok {
		for key := range metadata {
			info.DroppedMetadataKeys = append(info.DroppedMetadataKeys, key)
		}
		sort.Strings(info.DroppedMetadataKeys)
	}

	for _, field := range []string{
		"anthropic_version",
		"betas",
		"speed",
		"stop_sequences",
		"top_k",
	} {
		if _, ok := req[field]; ok {
			info.DroppedFields = append(info.DroppedFields, field)
		}
	}
	sort.Strings(info.DroppedFields)

	result, err := json.Marshal(responsesReq)
	if err != nil {
		return nil, info, fmt.Errorf("序列化 Responses 请求失败: %w", err)
	}

	return result, info, nil
}

// ConvertOpenAIResponsesToAnthropic 将 OpenAI Responses API 响应转换为 Anthropic Messages 响应
func ConvertOpenAIResponsesToAnthropic(body []byte) ([]byte, error) {
	resp, err := decodeJSONObject(body)
	if err != nil {
		return nil, fmt.Errorf("解析 Responses 响应失败: %w", err)
	}

	content := make([]interface{}, 0)
	sawToolUse := false

	for _, item := range asSlice(resp["output"]) {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		switch asString(itemMap["type"]) {
		case "reasoning":
			for _, summary := range asSlice(itemMap["summary"]) {
				summaryMap, ok := summary.(map[string]interface{})
				if !ok {
					continue
				}
				if text := asString(summaryMap["text"]); text != "" {
					content = append(content, map[string]interface{}{
						"type":     "thinking",
						"thinking": text,
					})
				}
			}

		case "message":
			for _, part := range asSlice(itemMap["content"]) {
				partMap, ok := part.(map[string]interface{})
				if !ok {
					continue
				}
				if asString(partMap["type"]) == "output_text" {
					content = append(content, map[string]interface{}{
						"type": "text",
						"text": asString(partMap["text"]),
					})
				}
			}

		case "function_call":
			content = append(content, map[string]interface{}{
				"type":  "tool_use",
				"id":    firstNonEmpty(asString(itemMap["call_id"]), asString(itemMap["id"])),
				"name":  asString(itemMap["name"]),
				"input": parseResponsesFunctionCallInput(itemMap["arguments"]),
			})
			sawToolUse = true
		}
	}

	stopReason := "end_turn"
	if sawToolUse {
		stopReason = "tool_use"
	}
	if asString(resp["status"]) == "incomplete" || asString(getNestedMapValue(resp, "incomplete_details", "reason")) == "max_output_tokens" {
		stopReason = "max_tokens"
	}

	usage := buildAnthropicUsageFromResponses(resp["usage"])

	anthropicResp := map[string]interface{}{
		"id":            firstNonEmpty(asString(resp["id"]), GenerateAnthropicMessageID()),
		"type":          "message",
		"role":          "assistant",
		"model":         firstNonEmpty(asString(resp["model"]), "unknown-model"),
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         usage,
	}

	result, err := json.Marshal(anthropicResp)
	if err != nil {
		return nil, fmt.Errorf("序列化 Anthropic 响应失败: %w", err)
	}

	return result, nil
}

func decodeJSONObject(body []byte) (map[string]interface{}, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	var result map[string]interface{}
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

func translateAnthropicSystemToInstructions(system interface{}) string {
	switch value := system.(type) {
	case string:
		return value
	case []interface{}:
		parts := make([]string, 0, len(value))
		for _, rawBlock := range value {
			block, ok := rawBlock.(map[string]interface{})
			if !ok {
				continue
			}
			if asString(block["type"]) == "text" {
				if text := asString(block["text"]); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func translateAnthropicMessagesToResponsesInput(messagesValue interface{}) ([]interface{}, error) {
	messageList, ok := messagesValue.([]interface{})
	if !ok {
		return nil, NewClientRequestRejectedError("messages 必须是数组")
	}

	items := make([]interface{}, 0, len(messageList))

	for i, rawMessage := range messageList {
		message, ok := rawMessage.(map[string]interface{})
		if !ok {
			return nil, NewClientRequestRejectedError(fmt.Sprintf("messages[%d] 格式无效", i))
		}

		role := asString(message["role"])
		content := message["content"]

		switch role {
		case "user":
			userItems, err := translateAnthropicMessageBlocksToResponses(i, role, content)
			if err != nil {
				return nil, err
			}
			items = append(items, userItems...)

		case "assistant":
			assistantItems, err := translateAnthropicMessageBlocksToResponses(i, role, content)
			if err != nil {
				return nil, err
			}
			items = append(items, assistantItems...)

		default:
			return nil, NewClientRequestRejectedError(
				fmt.Sprintf("messages[%d].role='%s' 不支持", i, role))
		}
	}

	return items, nil
}

func translateAnthropicMessageBlocksToResponses(messageIndex int, role string, content interface{}) ([]interface{}, error) {
	items := make([]interface{}, 0, 2)

	switch value := content.(type) {
	case string:
		if value == "" {
			return items, nil
		}
		partType := "input_text"
		if role == "assistant" {
			partType = "output_text"
		}
		items = append(items, map[string]interface{}{
			"type":    "message",
			"role":    role,
			"content": []interface{}{map[string]interface{}{"type": partType, "text": value}},
		})
		return items, nil

	case []interface{}:
		currentParts := make([]interface{}, 0, len(value))
		flushMessage := func() {
			if len(currentParts) == 0 {
				return
			}
			items = append(items, map[string]interface{}{
				"type":    "message",
				"role":    role,
				"content": append([]interface{}(nil), currentParts...),
			})
			currentParts = currentParts[:0]
		}

		for blockIndex, rawBlock := range value {
			block, ok := rawBlock.(map[string]interface{})
			if !ok {
				return nil, NewClientRequestRejectedError(
					fmt.Sprintf("messages[%d].content[%d] 格式无效", messageIndex, blockIndex))
			}

			switch role {
			case "user":
				switch asString(block["type"]) {
				case "text":
					currentParts = append(currentParts, map[string]interface{}{
						"type": "input_text",
						"text": asString(block["text"]),
					})

				case "image":
					imageURL, err := translateAnthropicImageSourceToURL(block["source"])
					if err != nil {
						return nil, fmt.Errorf("messages[%d].content[%d]: %w", messageIndex, blockIndex, err)
					}
					currentParts = append(currentParts, map[string]interface{}{
						"type":      "input_image",
						"image_url": imageURL,
					})

				case "tool_result":
					flushMessage()
					items = append(items, map[string]interface{}{
						"type":    "function_call_output",
						"call_id": asString(block["tool_use_id"]),
						"output":  stringifyAnthropicToolResultContent(block["content"]),
					})

				default:
					return nil, NewClientRequestRejectedError(
						fmt.Sprintf("messages[%d].content[%d].type='%s' 不支持", messageIndex, blockIndex, asString(block["type"])))
				}

			case "assistant":
				switch asString(block["type"]) {
				case "text":
					currentParts = append(currentParts, map[string]interface{}{
						"type": "output_text",
						"text": asString(block["text"]),
					})

				case "thinking":
					if thinking := asString(block["thinking"]); thinking != "" {
						currentParts = append(currentParts, map[string]interface{}{
							"type": "output_text",
							"text": thinking,
						})
					}

				case "tool_use":
					flushMessage()
					arguments, err := json.Marshal(block["input"])
					if err != nil {
						return nil, fmt.Errorf("messages[%d].content[%d].input 无法序列化: %w", messageIndex, blockIndex, err)
					}
					items = append(items, map[string]interface{}{
						"type":      "function_call",
						"call_id":   firstNonEmpty(asString(block["id"]), asString(block["tool_use_id"])),
						"name":      asString(block["name"]),
						"arguments": string(arguments),
					})

				default:
					return nil, NewClientRequestRejectedError(
						fmt.Sprintf("messages[%d].content[%d].type='%s' 不支持", messageIndex, blockIndex, asString(block["type"])))
				}
			}
		}

		flushMessage()
		return items, nil

	default:
		return nil, NewClientRequestRejectedError(
			fmt.Sprintf("messages[%d].content 格式无效", messageIndex))
	}
}

func translateAnthropicImageSourceToURL(sourceValue interface{}) (string, error) {
	source, ok := sourceValue.(map[string]interface{})
	if !ok {
		return "", NewClientRequestRejectedError("image.source 格式无效")
	}

	switch asString(source["type"]) {
	case "base64":
		data := asString(source["data"])
		if data == "" {
			return "", NewClientRequestRejectedError("image.source.data 不能为空")
		}
		mediaType := firstNonEmpty(asString(source["media_type"]), "image/jpeg")
		return fmt.Sprintf("data:%s;base64,%s", mediaType, data), nil

	case "url":
		if url := asString(source["url"]); url != "" {
			return url, nil
		}
		return "", NewClientRequestRejectedError("image.source.url 不能为空")

	default:
		return "", NewClientRequestRejectedError(
			fmt.Sprintf("image.source.type='%s' 不支持", asString(source["type"])))
	}
}

func translateAnthropicToolsToResponsesTools(toolsValue interface{}, opts ResponsesConvertOptions) ([]interface{}, error) {
	toolsList := asSlice(toolsValue)
	if len(toolsList) == 0 {
		return nil, nil
	}

	translated := make([]interface{}, 0, len(toolsList))
	for i, rawTool := range toolsList {
		tool, ok := rawTool.(map[string]interface{})
		if !ok {
			return nil, NewClientRequestRejectedError(fmt.Sprintf("tools[%d] 格式无效", i))
		}

		toolType := asString(tool["type"])
		toolName := asString(tool["name"])
		if strings.HasPrefix(toolType, "web_search") || toolName == "web_search" {
			if !opts.AllowWebSearch {
				providerLabel := "当前 OpenAI Compatible Claude 供应商"
				if providerName := strings.TrimSpace(opts.ProviderName); providerName != "" {
					providerLabel = fmt.Sprintf("供应商 %s", providerName)
				}
				return nil, NewClientRequestRejectedError(
					fmt.Sprintf("%s 未启用 Claude WebSearch 兼容；当前请求会映射为 Responses 工具类型 web_search_preview。若上游确实支持该工具，请在 provider 配置中设置 supportsWebSearch=true", providerLabel))
			}
			translated = append(translated, map[string]interface{}{"type": "web_search_preview"})
			continue
		}

		functionTool := map[string]interface{}{
			"type": "function",
			"name": toolName,
		}
		if description := asString(tool["description"]); description != "" {
			functionTool["description"] = description
		}
		if parameters, ok := tool["input_schema"]; ok && parameters != nil {
			functionTool["parameters"] = parameters
		}

		translated = append(translated, functionTool)
	}

	return translated, nil
}

func translateAnthropicToolChoiceToResponses(toolChoiceValue interface{}) (map[string]interface{}, error) {
	if toolChoiceValue == nil {
		return nil, nil
	}

	switch value := toolChoiceValue.(type) {
	case string:
		switch value {
		case "", "auto":
			return map[string]interface{}{"type": "auto"}, nil
		case "any":
			return map[string]interface{}{"type": "required"}, nil
		default:
			return map[string]interface{}{"type": "function", "name": value}, nil
		}

	case map[string]interface{}:
		switch asString(value["type"]) {
		case "", "auto":
			return map[string]interface{}{"type": "auto"}, nil
		case "any":
			return map[string]interface{}{"type": "required"}, nil
		case "tool":
			return map[string]interface{}{
				"type": "function",
				"name": asString(value["name"]),
			}, nil
		default:
			return nil, NewClientRequestRejectedError(
				fmt.Sprintf("tool_choice.type='%s' 不支持", asString(value["type"])))
		}

	default:
		return nil, NewClientRequestRejectedError("tool_choice 格式无效")
	}
}

func translateAnthropicThinkingToResponses(thinkingValue interface{}) map[string]interface{} {
	thinking, ok := thinkingValue.(map[string]interface{})
	if !ok || asString(thinking["type"]) != "enabled" {
		return nil
	}

	budget := asInt64(thinking["budget_tokens"])
	effort := "minimal"
	switch {
	case budget >= 10000:
		effort = "high"
	case budget >= 5000:
		effort = "medium"
	case budget >= 2000:
		effort = "low"
	}

	reasoning := map[string]interface{}{
		"effort":  effort,
		"summary": "detailed",
	}
	if summary := asString(thinking["summary"]); summary != "" {
		reasoning["summary"] = summary
	}

	return reasoning
}

func translateAnthropicOutputFormatToResponses(req map[string]interface{}) map[string]interface{} {
	outputFormat := req["output_format"]
	if outputFormat == nil {
		if outputConfig, ok := req["output_config"].(map[string]interface{}); ok {
			outputFormat = outputConfig["format"]
		}
	}

	formatMap, ok := outputFormat.(map[string]interface{})
	if !ok || asString(formatMap["type"]) != "json_schema" || formatMap["schema"] == nil {
		return nil
	}

	return map[string]interface{}{
		"format": map[string]interface{}{
			"type":   "json_schema",
			"name":   firstNonEmpty(asString(formatMap["name"]), "structured_output"),
			"schema": formatMap["schema"],
			"strict": true,
		},
	}
}

func translateAnthropicContextManagementToResponses(contextValue interface{}) []interface{} {
	if contextArray := asSlice(contextValue); len(contextArray) == 0 && contextArray != nil {
		return nil
	}

	contextManagement, ok := contextValue.(map[string]interface{})
	if !ok {
		return nil
	}

	edits := asSlice(contextManagement["edits"])
	if len(edits) == 0 {
		return nil
	}

	result := make([]interface{}, 0, len(edits))
	for _, rawEdit := range edits {
		edit, ok := rawEdit.(map[string]interface{})
		if !ok || asString(edit["type"]) != "compact_20260112" {
			continue
		}

		entry := map[string]interface{}{"type": "compaction"}
		if trigger, ok := edit["trigger"].(map[string]interface{}); ok && trigger["value"] != nil {
			entry["compact_threshold"] = trigger["value"]
		}
		result = append(result, entry)
	}

	if len(result) == 0 {
		return nil
	}

	return result
}

func stringifyAnthropicToolResultContent(content interface{}) string {
	switch value := content.(type) {
	case nil:
		return ""
	case string:
		return value
	case []interface{}:
		parts := make([]string, 0, len(value))
		allText := true
		for _, rawPart := range value {
			part, ok := rawPart.(map[string]interface{})
			if !ok || asString(part["type"]) != "text" {
				allText = false
				break
			}
			parts = append(parts, asString(part["text"]))
		}
		if allText {
			return strings.Join(parts, "\n")
		}
	}

	encoded, err := json.Marshal(content)
	if err != nil {
		return fmt.Sprintf("%v", content)
	}
	return string(encoded)
}

func parseResponsesFunctionCallInput(argumentsValue interface{}) map[string]interface{} {
	switch value := argumentsValue.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return map[string]interface{}{}
		}
		var parsed interface{}
		if err := json.Unmarshal([]byte(value), &parsed); err != nil {
			return map[string]interface{}{}
		}
		if parsedMap, ok := parsed.(map[string]interface{}); ok {
			return parsedMap
		}
		return map[string]interface{}{"value": parsed}

	case map[string]interface{}:
		return value

	default:
		return map[string]interface{}{}
	}
}

func buildAnthropicUsageFromResponses(usageValue interface{}) map[string]interface{} {
	inputTokens, outputTokens, cacheCreateTokens, cacheReadTokens, reasoningTokens := parseResponsesUsageValue(usageValue)
	usage := map[string]interface{}{
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
	}
	if cacheCreateTokens > 0 {
		usage["cache_creation_input_tokens"] = cacheCreateTokens
	}
	if cacheReadTokens > 0 {
		usage["cache_read_input_tokens"] = cacheReadTokens
	}
	if reasoningTokens > 0 {
		usage["output_tokens_details"] = map[string]interface{}{
			"reasoning_tokens": reasoningTokens,
		}
	}
	return usage
}

func parseResponsesUsageValue(usageValue interface{}) (inputTokens, outputTokens, cacheCreateTokens, cacheReadTokens, reasoningTokens int64) {
	usage, ok := usageValue.(map[string]interface{})
	if !ok {
		return 0, 0, 0, 0, 0
	}

	inputTokens = asInt64(usage["input_tokens"])
	outputTokens = asInt64(usage["output_tokens"])
	cacheCreateTokens = asInt64(usage["cache_creation_input_tokens"])
	cacheReadTokens = asInt64(usage["cache_read_input_tokens"])

	if inputDetails, ok := usage["input_tokens_details"].(map[string]interface{}); ok {
		if cacheCreateTokens == 0 {
			cacheCreateTokens = asInt64(inputDetails["cache_creation_input_tokens"])
		}
		if cacheReadTokens == 0 {
			cacheReadTokens = asInt64(inputDetails["cached_tokens"])
		}
	}

	if outputDetails, ok := usage["output_tokens_details"].(map[string]interface{}); ok {
		reasoningTokens = asInt64(outputDetails["reasoning_tokens"])
	}

	return inputTokens, outputTokens, cacheCreateTokens, cacheReadTokens, reasoningTokens
}

func asString(value interface{}) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case json.Number:
		return v.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

func asInt64(value interface{}) int64 {
	switch v := value.(type) {
	case nil:
		return 0
	case int:
		return int64(v)
	case int32:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
		if f, err := v.Float64(); err == nil {
			return int64(f)
		}
	case string:
		var n json.Number = json.Number(v)
		if parsed, err := n.Int64(); err == nil {
			return parsed
		}
	}
	return 0
}

func asSlice(value interface{}) []interface{} {
	if value == nil {
		return nil
	}
	if result, ok := value.([]interface{}); ok {
		return result
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func getNestedMapValue(root map[string]interface{}, parentKey, childKey string) interface{} {
	parent, ok := root[parentKey].(map[string]interface{})
	if !ok {
		return nil
	}
	return parent[childKey]
}

// ResponsesToAnthropicSSEConverter 将 OpenAI Responses SSE 转为 Anthropic SSE
type ResponsesToAnthropicSSEConverter struct {
	messageID        string
	model            string
	sentMessageStart bool
	sentMessageStop  bool
	nextBlockIndex   int
	itemToBlockIndex map[string]int
	openBlockIndexes map[int]struct{}
}

func NewResponsesToAnthropicSSEConverter(model string) *ResponsesToAnthropicSSEConverter {
	return &ResponsesToAnthropicSSEConverter{
		messageID:        GenerateAnthropicMessageID(),
		model:            model,
		nextBlockIndex:   0,
		itemToBlockIndex: make(map[string]int),
		openBlockIndexes: make(map[int]struct{}),
	}
}

func (c *ResponsesToAnthropicSSEConverter) ProcessLine(line string) string {
	if c.sentMessageStop {
		return ""
	}

	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "event:") || !strings.HasPrefix(line, "data:") {
		return ""
	}

	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" {
		return ""
	}
	if data == "[DONE]" {
		return c.emitCompletion(gjson.Result{}, "end_turn")
	}

	parsed := gjson.Parse(data)
	eventType := parsed.Get("type").String()
	if eventType == "" {
		return ""
	}

	if model := parsed.Get("response.model").String(); model != "" && c.model == "" {
		c.model = model
	}

	switch eventType {
	case "response.created":
		return c.ensureMessageStart()

	case "response.output_item.added":
		var output strings.Builder
		output.WriteString(c.ensureMessageStart())

		item := parsed.Get("item")
		itemType := item.Get("type").String()
		itemID := item.Get("id").String()

		switch itemType {
		case "message":
			output.WriteString(c.emitBlockStart(itemID, map[string]interface{}{
				"type": "text",
				"text": "",
			}))
		case "function_call":
			output.WriteString(c.emitBlockStart(itemID, map[string]interface{}{
				"type":  "tool_use",
				"id":    firstNonEmpty(item.Get("call_id").String(), itemID),
				"name":  item.Get("name").String(),
				"input": map[string]interface{}{},
			}))
		case "reasoning":
			output.WriteString(c.emitBlockStart(itemID, map[string]interface{}{
				"type":     "thinking",
				"thinking": "",
			}))
		}

		return output.String()

	case "response.output_text.delta":
		itemID := parsed.Get("item_id").String()
		index, start := c.ensureBlock(itemID, map[string]interface{}{
			"type": "text",
			"text": "",
		})

		var output strings.Builder
		output.WriteString(c.ensureMessageStart())
		output.WriteString(start)
		output.WriteString(c.emitContentBlockDelta(index, map[string]interface{}{
			"type": "text_delta",
			"text": parsed.Get("delta").String(),
		}))
		return output.String()

	case "response.reasoning_summary_text.delta":
		itemID := parsed.Get("item_id").String()
		index, start := c.ensureBlock(itemID, map[string]interface{}{
			"type":     "thinking",
			"thinking": "",
		})

		var output strings.Builder
		output.WriteString(c.ensureMessageStart())
		output.WriteString(start)
		output.WriteString(c.emitContentBlockDelta(index, map[string]interface{}{
			"type":     "thinking_delta",
			"thinking": parsed.Get("delta").String(),
		}))
		return output.String()

	case "response.function_call_arguments.delta":
		itemID := parsed.Get("item_id").String()
		index, start := c.ensureBlock(itemID, map[string]interface{}{
			"type":  "tool_use",
			"id":    firstNonEmpty(parsed.Get("call_id").String(), itemID),
			"name":  parsed.Get("name").String(),
			"input": map[string]interface{}{},
		})

		var output strings.Builder
		output.WriteString(c.ensureMessageStart())
		output.WriteString(start)
		output.WriteString(c.emitContentBlockDelta(index, map[string]interface{}{
			"type":         "input_json_delta",
			"partial_json": parsed.Get("delta").String(),
		}))
		return output.String()

	case "response.output_item.done":
		itemID := parsed.Get("item.id").String()
		if itemID == "" {
			return ""
		}
		if index, ok := c.itemToBlockIndex[itemID]; ok {
			return c.emitContentBlockStop(index)
		}
		return ""

	case "response.completed":
		return c.emitCompletion(parsed.Get("response"), "end_turn")

	case "response.incomplete":
		return c.emitCompletion(parsed.Get("response"), "max_tokens")

	case "response.failed":
		return c.emitCompletion(parsed.Get("response"), "end_turn")
	}

	return ""
}

func (c *ResponsesToAnthropicSSEConverter) ensureMessageStart() string {
	if c.sentMessageStart {
		return ""
	}
	c.sentMessageStart = true

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
				"input_tokens":                0,
				"output_tokens":               0,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
			},
		},
	})
}

func (c *ResponsesToAnthropicSSEConverter) emitBlockStart(itemID string, contentBlock map[string]interface{}) string {
	index := c.nextBlockIndex
	c.nextBlockIndex++
	if itemID != "" {
		c.itemToBlockIndex[itemID] = index
	}
	c.openBlockIndexes[index] = struct{}{}

	return c.emitAnthropicSSE("content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         index,
		"content_block": contentBlock,
	})
}

func (c *ResponsesToAnthropicSSEConverter) ensureBlock(itemID string, contentBlock map[string]interface{}) (int, string) {
	if itemID != "" {
		if index, ok := c.itemToBlockIndex[itemID]; ok {
			return index, ""
		}
	}
	index := c.nextBlockIndex
	return index, c.emitBlockStart(itemID, contentBlock)
}

func (c *ResponsesToAnthropicSSEConverter) emitContentBlockDelta(index int, delta map[string]interface{}) string {
	return c.emitAnthropicSSE("content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": index,
		"delta": delta,
	})
}

func (c *ResponsesToAnthropicSSEConverter) emitContentBlockStop(index int) string {
	if _, ok := c.openBlockIndexes[index]; !ok {
		return ""
	}
	delete(c.openBlockIndexes, index)

	return c.emitAnthropicSSE("content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": index,
	})
}

func (c *ResponsesToAnthropicSSEConverter) emitCompletion(response gjson.Result, fallbackStopReason string) string {
	if c.sentMessageStop {
		return ""
	}

	if model := response.Get("model").String(); model != "" && c.model == "" {
		c.model = model
	}

	inputTokens, outputTokens, cacheCreateTokens, cacheReadTokens, reasoningTokens := parseResponsesUsageResult(response.Get("usage"))
	stopReason := fallbackStopReason
	if response.Exists() {
		if response.Get("output.#(type==\"function_call\")").Exists() {
			stopReason = "tool_use"
		}
		if response.Get("status").String() == "incomplete" || response.Get("incomplete_details.reason").String() == "max_output_tokens" {
			stopReason = "max_tokens"
		}
	}

	var output strings.Builder
	output.WriteString(c.ensureMessageStart())
	output.WriteString(c.emitOpenBlockStops())

	usage := map[string]interface{}{
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
	}
	if cacheCreateTokens > 0 {
		usage["cache_creation_input_tokens"] = cacheCreateTokens
	}
	if cacheReadTokens > 0 {
		usage["cache_read_input_tokens"] = cacheReadTokens
	}
	if reasoningTokens > 0 {
		usage["output_tokens_details"] = map[string]interface{}{
			"reasoning_tokens": reasoningTokens,
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

	c.sentMessageStop = true
	return output.String()
}

func (c *ResponsesToAnthropicSSEConverter) emitOpenBlockStops() string {
	if len(c.openBlockIndexes) == 0 {
		return ""
	}

	indexes := make([]int, 0, len(c.openBlockIndexes))
	for index := range c.openBlockIndexes {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	var output strings.Builder
	for _, index := range indexes {
		output.WriteString(c.emitContentBlockStop(index))
	}
	return output.String()
}

func (c *ResponsesToAnthropicSSEConverter) emitAnthropicSSE(event string, payload interface{}) string {
	body, _ := json.Marshal(payload)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(body))
}

func parseResponsesUsageResult(usage gjson.Result) (inputTokens, outputTokens, cacheCreateTokens, cacheReadTokens, reasoningTokens int64) {
	if !usage.Exists() {
		return 0, 0, 0, 0, 0
	}

	inputTokens = usage.Get("input_tokens").Int()
	outputTokens = usage.Get("output_tokens").Int()
	cacheCreateTokens = usage.Get("cache_creation_input_tokens").Int()
	if cacheCreateTokens == 0 {
		cacheCreateTokens = usage.Get("input_tokens_details.cache_creation_input_tokens").Int()
	}
	cacheReadTokens = usage.Get("cache_read_input_tokens").Int()
	if cacheReadTokens == 0 {
		cacheReadTokens = usage.Get("input_tokens_details.cached_tokens").Int()
	}
	reasoningTokens = usage.Get("output_tokens_details.reasoning_tokens").Int()
	return inputTokens, outputTokens, cacheCreateTokens, cacheReadTokens, reasoningTokens
}
