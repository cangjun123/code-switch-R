package services

// gjson 热路径改写（HasCodexMultiAgentConflict / codexRequestToolOutputIDs /
// estimateInputTokens）的等价性对照测试：旧 JSON 树实现保留为 test helper，
// 表驱动断言新旧实现输出完全一致。

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// --- 旧实现（JSON 树版本，仅测试用） ---

func hasCodexMultiAgentNamespaceConflictJSONTree(body []byte) (bool, error) {
	root, err := decodeJSONPreservingNumbers(body)
	if err != nil {
		return false, err
	}
	definitions := codexNamespaceDefinitions{}
	inspectCodexNamespaceDefinitions(root, &definitions)
	return definitions.collaboration && definitions.agents, nil
}

func codexRequestToolOutputIDsJSONTree(body []byte) (map[string]struct{}, bool) {
	result := make(map[string]struct{})
	root, err := decodeJSONPreservingNumbers(body)
	if err != nil {
		return result, false
	}
	object, ok := root.(map[string]any)
	if !ok {
		return result, false
	}
	input, ok := object["input"].([]any)
	if !ok {
		return result, false
	}
	hasOutput := false
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok || !isCodexToolOutputType(stringField(item, "type")) {
			continue
		}
		hasOutput = true
		if callID := stringField(item, "call_id"); callID != "" {
			result[callID] = struct{}{}
		}
	}
	return result, hasOutput
}

func estimateInputTokensLegacy(bodyBytes []byte) int {
	var body struct {
		System   interface{}     `json:"system"`
		Messages []interface{}   `json:"messages"`
		Tools    json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		return 100
	}

	var totalChars int
	var cjkCount int

	extractText := func(v interface{}) string {
		if v == nil {
			return ""
		}
		switch val := v.(type) {
		case string:
			return val
		case []interface{}:
			var parts []string
			for _, item := range val {
				if s, ok := item.(string); ok {
					parts = append(parts, s)
				} else if m, ok := item.(map[string]interface{}); ok {
					if t, ok := m["type"].(string); ok && t == "text" {
						parts = append(parts, fmt.Sprint(m["text"]))
					}
				}
			}
			return strings.Join(parts, "\n")
		default:
			return fmt.Sprint(v)
		}
	}

	systemText := extractText(body.System)
	for _, ch := range systemText {
		if ch >= 0x4e00 && ch <= 0x9fff {
			cjkCount++
		}
		totalChars += len(string(ch))
	}

	for _, raw := range body.Messages {
		if m, ok := raw.(map[string]interface{}); ok {
			txt := fmt.Sprint(m["role"]) + "\n" + extractText(m["content"])
			for _, ch := range txt {
				if ch >= 0x4e00 && ch <= 0x9fff {
					cjkCount++
				}
				totalChars += len(string(ch))
			}
		}
	}

	if body.Tools != nil {
		totalChars += len(body.Tools)
	}

	otherCount := totalChars - cjkCount
	if otherCount < 0 {
		otherCount = 0
	}
	estimated := cjkCount + (otherCount / 4) + 20
	if estimated < 1 {
		estimated = 1
	}
	return estimated
}

// --- HasCodexMultiAgentNamespaceConflict 对照 ---

func TestHasCodexMultiAgentNamespaceConflictEquivalence(t *testing.T) {
	namespacedTools := func(name string) string {
		return `{"type":"namespace","name":"` + name + `"}`
	}
	cases := []string{
		// 正常：conflict 有/无
		`{"tools":[` + namespacedTools("collaboration") + `,` + namespacedTools("agents") + `]}`,
		`{"tools":[` + namespacedTools("collaboration") + `]}`,
		`{"tools":[` + namespacedTools("agents") + `]}`,
		`{"additional_tools":[` + namespacedTools("collaboration") + `,` + namespacedTools("agents") + `]}`,
		// input 内 additional_tools 项
		`{"input":[{"type":"additional_tools","tools":[` + namespacedTools("collaboration") + `]},` + namespacedTools("agents") + `]}`,
		`{"input":[{"type":"message","tools":[` + namespacedTools("collaboration") + `]}]}`,
		// namespace 定义的对象/items 包装形式
		`{"tools":{"items":[` + namespacedTools("collaboration") + `,` + namespacedTools("agents") + `]}}`,
		`{"tools":{"items":{"items":[` + namespacedTools("collaboration") + `,` + namespacedTools("agents") + `]}}}`,
		`{"tools":{"items":null}}`,
		`{"tools":null}`,
		// 非 namespace 类型的定义不触发
		`{"tools":[{"type":"function","name":"collaboration"},{"type":"function","name":"agents"}]}`,
		// 非 object 根
		`[]`, `"string"`, `123`, `true`, `null`, ``,
		// input 非数组 / 缺失
		`{"input":{"a":1}}`, `{"input":"x"}`, `{}`,
		// 畸形 JSON
		`{"tools":[`, `{"a":1} garbage`, `{"a":1}{"b":2}`, `{"a":1} 5`,
	}
	for _, body := range cases {
		newConflict, newErr := HasCodexMultiAgentNamespaceConflict([]byte(body))
		oldConflict, oldErr := hasCodexMultiAgentNamespaceConflictJSONTree([]byte(body))
		if newConflict != oldConflict {
			t.Errorf("body=%q conflict mismatch: gjson=%v jsonTree=%v", body, newConflict, oldConflict)
		}
		if (newErr == nil) != (oldErr == nil) {
			t.Errorf("body=%q error mismatch: gjsonErr=%v jsonTreeErr=%v", body, newErr, oldErr)
		}
	}
}

// --- codexRequestToolOutputIDs 对照 ---

func TestCodexRequestToolOutputIDsEquivalence(t *testing.T) {
	cases := []string{
		// 正常收集
		`{"input":[{"type":"function_call_output","call_id":"a"},{"type":"function_call_output","call_id":"b"}]}`,
		`{"input":[{"type":"custom_tool_call_output","call_id":"a"}]}`,
		`{"input":[{"type":"weird_call_output","call_id":"a"}]}`,
		// 类型陷阱：type/call_id 非字符串
		`{"input":[{"type":"function_call_output","call_id":123}]}`,
		`{"input":[{"type":"function_call_output","call_id":true}]}`,
		`{"input":[{"type":"function_call_output","call_id":null}]}`,
		`{"input":[{"type":"function_call_output","call_id":{"x":"a"}}]}`,
		`{"input":[{"type":123,"call_id":"a"}]}`,
		`{"input":[{"type":null,"call_id":"a"}]}`,
		`{"input":[{"type":true,"call_id":"a"}]}`,
		// call_id 空串
		`{"input":[{"type":"function_call_output","call_id":""}]}`,
		// 大小写与后缀变体
		`{"input":[{"type":"FUNCTION_CALL_OUTPUT","call_id":"a"}]}`,
		`{"input":[{"type":" function_call_output ","call_id":"a"}]}`,
		// input 项非 object
		`{"input":["plain","x",5,null,true,[]]}`,
		// 非 object 根 / input 非数组 / 缺失
		`[]`, `"s"`, `1`, `null`, `true`, ``, `{}`, `{"input":"x"}`, `{"input":{}}`,
		// 畸形 JSON
		`{"input":[`, `{} trailing`, `{} {}`, `{"input":[]} extra`,
		// 重复 call_id
		`{"input":[{"type":"function_call_output","call_id":"a"},{"type":"function_call_output","call_id":"a"}]}`,
	}
	for _, body := range cases {
		newIDs, newHas := codexRequestToolOutputIDs([]byte(body))
		oldIDs, oldHas := codexRequestToolOutputIDsJSONTree([]byte(body))
		if newHas != oldHas {
			t.Errorf("body=%q hasOutput mismatch: gjson=%v jsonTree=%v", body, newHas, oldHas)
		}
		if len(newIDs) != len(oldIDs) {
			t.Errorf("body=%q ids size mismatch: gjson=%v jsonTree=%v", body, newIDs, oldIDs)
			continue
		}
		for id := range oldIDs {
			if _, ok := newIDs[id]; !ok {
				t.Errorf("body=%q missing id %q in gjson result", body, id)
			}
		}
	}
}

// --- estimateInputTokens 对照 ---

func TestEstimateInputTokensEquivalence(t *testing.T) {
	cases := []string{
		// 基本 system/messages/tools
		`{"system":"You are helpful","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"custom"}]}`,
		// system 数组形式（content blocks）
		`{"system":[{"type":"text","text":"line1"},{"type":"text","text":"中文行"}],"messages":[]}`,
		`{"system":[{"type":"other","text":"ignored"},{"type":"text","text":"kept"}],"messages":[]}`,
		`{"system":[{"type":"text","text":123}],"messages":[]}`,
		`{"system":[{"type":"text"}],"messages":[]}`,
		`{"system":null,"messages":[]}`,
		// content 混合块
		`{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"image","text":"b"},{"plain string"},{"type":"text","text":true}]}]}`,
		`{"messages":[{"role":"assistant","content":"plain text"}]}`,
		`{"messages":[{"role":"user","content":123}]}`,
		`{"messages":[{"role":"user","content":null}]}`,
		`{"messages":[{"role":"user"}]}`,
		// 中文 / 混合
		`{"messages":[{"role":"user","content":"你好世界"}]}`,
		`{"system":"系统提示","messages":[{"role":"user","content":"hello 你好"}]}`,
		// messages 项非 object
		`{"messages":["str",5,null,true,[]]}`,
		// messages 非数组 → 100（UnmarshalTypeError 等价）
		`{"messages":"not-array"}`, `{"messages":{}}`, `{"messages":5}`,
		// tools 变体（len(Raw) 对齐）
		`{"tools":null,"messages":[]}`,
		`{"tools":[ ],"messages":[]}`,
		`{"tools":[{"a":1},{"b":[1,2]}],"messages":[]}`,
		// 畸形 JSON → 100
		`{"messages":[`, `garbage`, `{} {}`, ``,
		// 其他顶层字段被忽略
		`{"model":"x","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
	}
	for _, body := range cases {
		got := estimateInputTokens([]byte(body))
		want := estimateInputTokensLegacy([]byte(body))
		if got != want {
			t.Errorf("body=%q tokens mismatch: gjson=%d legacy=%d", body, got, want)
		}
	}
}
