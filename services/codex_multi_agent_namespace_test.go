package services

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

func TestRewriteCodexMultiAgentRequest(t *testing.T) {
	body := []byte(`{
  "model":"gpt-5.4",
  "instructions":"Keep the collaboration namespace name in this text.",
  "tools":[
    {"type":"namespace","name":"collaboration","description":"collaboration stays in descriptions"},
    {"type":"function","name":"collaboration"}
  ],
  "input":[
	{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"collaboration"}]},
    {"type":"function_call","namespace":"collaboration","name":"spawn_agent","arguments":{"type":"function_call","namespace":"collaboration"}},
    {"type":"custom_tool_call","namespace":"collaboration","name":"send_message","input":{"type":"custom_tool_call","namespace":"collaboration"}},
    {"type":"message","namespace":"collaboration","content":"collaboration"}
  ]
}`)

	rewritten, count, err := RewriteCodexMultiAgentRequest(body)
	if err != nil {
		t.Fatalf("RewriteCodexMultiAgentRequest() error = %v", err)
	}
	if count != 4 {
		t.Fatalf("modified count = %d, want 4", count)
	}

	result := gjson.ParseBytes(rewritten)
	for _, path := range []string{"tools.0.name", "input.0.tools.0.name", "input.1.namespace", "input.2.namespace"} {
		if got := result.Get(path).String(); got != "agents" {
			t.Errorf("%s = %q, want agents", path, got)
		}
	}
	if got := result.Get("tools.1.name").String(); got != "collaboration" {
		t.Errorf("non-namespace tool name changed to %q", got)
	}
	if got := result.Get("instructions").String(); got != "Keep the collaboration namespace name in this text." {
		t.Errorf("instructions changed to %q", got)
	}
	if got := result.Get("input.1.arguments.namespace").String(); got != "collaboration" {
		t.Errorf("arguments namespace changed to %q", got)
	}
	if got := result.Get("input.2.input.namespace").String(); got != "collaboration" {
		t.Errorf("custom tool input namespace changed to %q", got)
	}
	if got := result.Get("input.3.namespace").String(); got != "collaboration" {
		t.Errorf("ordinary namespace field changed to %q", got)
	}
}

func TestRewriteCodexMultiAgentRequestNoMatchReturnsOriginalBytes(t *testing.T) {
	body := []byte("{ \n  \"tools\": [{\"type\":\"namespace\",\"name\":\"agents\"}], \n  \"input\": \"collaboration\"\n}\n")

	rewritten, count, err := RewriteCodexMultiAgentRequest(body)
	if err != nil {
		t.Fatalf("RewriteCodexMultiAgentRequest() error = %v", err)
	}
	if count != 0 {
		t.Fatalf("modified count = %d, want 0", count)
	}
	if !bytes.Equal(rewritten, body) {
		t.Fatalf("no-match request bytes changed:\n got: %q\nwant: %q", rewritten, body)
	}
}

func TestSanitizeCodexProviderBoundHistory(t *testing.T) {
	body := []byte(`{
  "previous_response_id":"resp_foreign",
  "conversation":"conv_foreign",
  "input":[
    {"type":"reasoning","id":"rs_foreign","summary":[],"encrypted_content":"ciphertext"},
    {"type":"item_reference","id":"item_foreign"},
    {"type":"message","id":"msg_foreign","role":"assistant","content":[
      {"type":"output_text","text":"keep this message"},
      {"type":"encrypted_content","encrypted_content":"ciphertext"}
    ]},
    {"type":"function_call","id":"fc_foreign","namespace":"agents","name":"spawn_agent","call_id":"call_1","arguments":"{\"id\":\"keep inside arguments\"}"},
    {"type":"function_call_output","id":"out_foreign","call_id":"call_1","output":"done"},
	{"type":"custom_tool_call_output","id":"custom_out_foreign","call_id":"call_2","encrypted_content":"item-ciphertext","output":[
	  {"type":"input_text","text":"keep structured output"},
	  {"type":"encrypted_content","encrypted_content":"ciphertext"}
	]},
    {"type":"compaction","encrypted_content":"ciphertext"}
  ]
}`)

	sanitized, stats, err := SanitizeCodexProviderBoundHistory(body)
	if err != nil {
		t.Fatalf("SanitizeCodexProviderBoundHistory() error = %v", err)
	}
	if stats.RemovedItems != 3 || stats.RemovedContentParts != 3 || stats.RemovedItemIDs != 4 || stats.RemovedTopLevelRefs != 2 {
		t.Fatalf("unexpected sanitize stats: %+v", stats)
	}
	result := gjson.ParseBytes(sanitized)
	if result.Get("previous_response_id").Exists() || result.Get("conversation").Exists() {
		t.Fatalf("provider-bound top-level references remain: %s", sanitized)
	}
	if got := result.Get("input.#").Int(); got != 4 {
		t.Fatalf("sanitized input item count = %d, want 4: %s", got, sanitized)
	}
	if got := result.Get("input.0.content.#").Int(); got != 1 {
		t.Fatalf("message content count = %d, want 1: %s", got, sanitized)
	}
	if got := result.Get("input.0.content.0.text").String(); got != "keep this message" {
		t.Fatalf("message text = %q", got)
	}
	for index := 0; index < 4; index++ {
		if result.Get(fmt.Sprintf("input.%d.id", index)).Exists() {
			t.Fatalf("provider-bound input id remains at index %d: %s", index, sanitized)
		}
	}
	if got := result.Get("input.1.namespace").String(); got != "agents" {
		t.Fatalf("function call namespace = %q", got)
	}
	if got := result.Get("input.1.arguments").String(); got != "{\"id\":\"keep inside arguments\"}" {
		t.Fatalf("function arguments changed to %q", got)
	}
	if got := result.Get("input.3.output.#").Int(); got != 1 {
		t.Fatalf("custom tool output count = %d, want 1: %s", got, sanitized)
	}
	if got := result.Get("input.3.output.0.text").String(); got != "keep structured output" {
		t.Fatalf("custom tool output text changed to %q", got)
	}
	if result.Get("input.3.encrypted_content").Exists() {
		t.Fatalf("custom tool output retained item-level ciphertext: %s", sanitized)
	}
}

func TestSanitizeCodexProviderBoundHistoryNoMatchAndInvalidJSON(t *testing.T) {
	body := []byte("{ \n  \"input\": [{\"type\":\"message\",\"content\":\"keep\"}]\n}\n")
	sanitized, stats, err := SanitizeCodexProviderBoundHistory(body)
	if err != nil || stats.Total() != 0 || !bytes.Equal(sanitized, body) {
		t.Fatalf("no-match sanitize = (%q, %+v, %v)", sanitized, stats, err)
	}

	invalid := []byte("{\"input\":[")
	sanitized, stats, err = SanitizeCodexProviderBoundHistory(invalid)
	if err == nil || stats.Total() != 0 || !bytes.Equal(sanitized, invalid) {
		t.Fatalf("invalid sanitize = (%q, %+v, %v)", sanitized, stats, err)
	}
}

func TestRewriteCodexMultiAgentRequestDoesNotInspectToolSchemas(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"example","parameters":{"properties":{"definition":{"default":{"type":"namespace","name":"collaboration"}},"call":{"default":{"type":"function_call","namespace":"collaboration"}}}}}]}`)

	rewritten, count, err := RewriteCodexMultiAgentRequest(body)
	if err != nil {
		t.Fatalf("RewriteCodexMultiAgentRequest() error = %v", err)
	}
	if count != 0 || !bytes.Equal(rewritten, body) {
		t.Fatalf("tool schema must remain untouched: count=%d body=%q", count, rewritten)
	}
}

func TestRewriteCodexMultiAgentRequestRejectsConflictingDefinitions(t *testing.T) {
	body := []byte(`{"tools":[{"type":"namespace","name":"collaboration"}],"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"agents"}]}]}`)

	rewritten, count, err := RewriteCodexMultiAgentRequest(body)
	if !errors.Is(err, ErrCodexMultiAgentNamespaceConflict) {
		t.Fatalf("error = %v, want ErrCodexMultiAgentNamespaceConflict", err)
	}
	if count != 0 || !bytes.Equal(rewritten, body) {
		t.Fatalf("conflicting request must remain untouched: count=%d body=%q", count, rewritten)
	}
}

func TestRewriteCodexMultiAgentRequestInvalidJSON(t *testing.T) {
	body := []byte(`{"tools":[`)

	rewritten, count, err := RewriteCodexMultiAgentRequest(body)
	if err == nil {
		t.Fatal("expected invalid JSON error")
	}
	if count != 0 || !bytes.Equal(rewritten, body) {
		t.Fatalf("invalid request must remain untouched: count=%d body=%q", count, rewritten)
	}
}

func TestRewriteCodexMultiAgentResponse(t *testing.T) {
	body := []byte(`{"id":9007199254740993,"output":[{"type":"function_call","namespace":"agents","name":"spawn_agent"},{"type":"custom_tool_call","namespace":"agents","name":"send_message"},{"type":"message","namespace":"agents","content":[{"text":"agents"}]}]}`)

	rewritten, count, err := RewriteCodexMultiAgentResponse(body)
	if err != nil {
		t.Fatalf("RewriteCodexMultiAgentResponse() error = %v", err)
	}
	if count != 2 {
		t.Fatalf("modified count = %d, want 2", count)
	}
	result := gjson.ParseBytes(rewritten)
	if got := result.Get("output.0.namespace").String(); got != "collaboration" {
		t.Errorf("function_call namespace = %q", got)
	}
	if got := result.Get("output.1.namespace").String(); got != "collaboration" {
		t.Errorf("custom_tool_call namespace = %q", got)
	}
	if got := result.Get("output.2.namespace").String(); got != "agents" {
		t.Errorf("ordinary namespace field changed to %q", got)
	}
	if got := result.Get("id").Raw; got != "9007199254740993" {
		t.Errorf("large integer changed to %q", got)
	}
}

func TestRewriteCodexMultiAgentResponseNoMatchAndInvalidJSON(t *testing.T) {
	for _, test := range []struct {
		name    string
		body    []byte
		wantErr bool
	}{
		{name: "no match", body: []byte("{ \"output\": [{\"type\":\"function_call\",\"namespace\":\"collaboration\"}] }\n")},
		{name: "invalid JSON", body: []byte(`{"output":[`), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			rewritten, count, err := RewriteCodexMultiAgentResponse(test.body)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, test.wantErr)
			}
			if count != 0 || !bytes.Equal(rewritten, test.body) {
				t.Fatalf("unmodified response bytes changed: count=%d body=%q", count, rewritten)
			}
		})
	}
}

func TestCodexMultiAgentNamespaceSSEHook(t *testing.T) {
	modified := 0
	hook := NewCodexMultiAgentNamespaceSSEHook(&modified)
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "output item added",
			line: `data: {"type":"response.output_item.added","item":{"type":"function_call","namespace":"agents"}}`,
			want: `data: {"item":{"namespace":"collaboration","type":"function_call"},"type":"response.output_item.added"}`,
		},
		{
			name: "output item done",
			line: "data:\t{\"type\":\"response.output_item.done\",\"item\":{\"type\":\"custom_tool_call\",\"namespace\":\"agents\"}}\r",
			want: "data:\t{\"item\":{\"namespace\":\"collaboration\",\"type\":\"custom_tool_call\"},\"type\":\"response.output_item.done\"}\r",
		},
		{
			name: "response completed",
			line: `data: {"type":"response.completed","response":{"output":[{"type":"function_call","namespace":"agents"}]}}`,
			want: `data: {"response":{"output":[{"namespace":"collaboration","type":"function_call"}]},"type":"response.completed"}`,
		},
		{name: "done sentinel", line: "data: [DONE]", want: "data: [DONE]"},
		{name: "event line", line: "event: response.completed", want: "event: response.completed"},
		{name: "non JSON", line: "data: not-json", want: "data: not-json"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			flush, got := hook([]byte(test.line))
			if !flush {
				t.Fatal("hook unexpectedly suppressed line")
			}
			if string(got) != test.want {
				t.Fatalf("line = %q, want %q", got, test.want)
			}
		})
	}
	if modified != 3 {
		t.Fatalf("modified count = %d, want 3", modified)
	}
}
