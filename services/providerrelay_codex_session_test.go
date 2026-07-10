package services

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
)

func codexSessionSSE(output string) string {
	return strings.Join([]string{
		"event: response.output_item.added",
		"data: {\"type\":\"response.output_item.added\",\"item\":" + output + "}",
		"",
		"event: response.completed",
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[" + output + "],\"usage\":{\"input_tokens\":12,\"output_tokens\":4}}}",
		"",
		"data: [DONE]",
		"",
	}, "\n")
}

func codexSessionRequest(input string) []byte {
	return []byte(`{"model":"gpt-5.6-sol","stream":true,"prompt_cache_key":"switch-session","tools":[{"type":"namespace","name":"collaboration"}],"input":` + input + `}`)
}

func TestCodexProviderSessionPinsToolResultThenSanitizesReverseSwitch(t *testing.T) {
	type upstreamCapture struct {
		mu      sync.Mutex
		bodies  [][]byte
		headers []http.Header
	}
	add := func(capture *upstreamCapture, r *http.Request) []byte {
		body, _ := io.ReadAll(r.Body)
		capture.mu.Lock()
		capture.bodies = append(capture.bodies, append([]byte(nil), body...))
		capture.headers = append(capture.headers, r.Header.Clone())
		capture.mu.Unlock()
		return body
	}

	var etoCapture, plainCapture upstreamCapture
	eto := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := add(&etoCapture, r)
		w.Header().Set("Content-Type", "text/event-stream")
		if gjson.GetBytes(body, `input.#(type=="custom_tool_call_output")`).Exists() {
			_, _ = w.Write([]byte(codexSessionSSE(`{"type":"message","id":"msg_eto","role":"assistant","content":[{"type":"output_text","text":"tool result accepted"}]}`)))
			return
		}
		_, _ = w.Write([]byte(codexSessionSSE(`{"type":"custom_tool_call","id":"ctc_eto","call_id":"call_eto","namespace":"agents","name":"exec","input":"{}"}`)))
	}))
	defer eto.Close()

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := add(&plainCapture, r)
		if bytes.Contains(body, []byte("eto-ciphertext")) ||
			gjson.GetBytes(body, `input.#(id=="ctc_eto")`).Exists() ||
			gjson.GetBytes(body, `input.#(id=="msg_eto")`).Exists() {
			http.Error(w, "Encrypted function output content could not be decrypted or decoded", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codexSessionSSE(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"switched safely"}]}`)))
	}))
	defer plain.Close()

	providers, relay := newTestRelayService(t)
	providerList := []Provider{
		{ID: 1, Name: "eto", APIURL: eto.URL, APIKey: "eto-key", Enabled: true, Level: 1, CodexMultiAgentNamespaceRewrite: true},
		{ID: 2, Name: "plain", APIURL: plain.URL, APIKey: "plain-key", Enabled: false, Level: 1},
	}
	if err := providers.SaveProviders(ProviderKindCodex, providerList); err != nil {
		t.Fatalf("SaveProviders initial: %v", err)
	}

	first := performCodexNamespaceTestRequest(t, relay, codexSessionRequest(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"run a tool"}]}]`))
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"namespace":"collaboration"`) {
		t.Fatalf("first response status=%d body=%s", first.Code, first.Body.String())
	}

	providerList[0].Enabled = false
	providerList[1].Enabled = true
	if err := providers.SaveProviders(ProviderKindCodex, providerList); err != nil {
		t.Fatalf("SaveProviders switch: %v", err)
	}
	toolResultInput := `[
	  {"type":"reasoning","id":"rs_eto","encrypted_content":"eto-ciphertext"},
	  {"type":"custom_tool_call","id":"ctc_eto","call_id":"call_eto","namespace":"collaboration","name":"exec","input":"{}"},
	  {"type":"custom_tool_call_output","call_id":"call_eto","output":[{"type":"input_text","text":"tool output"},{"type":"encrypted_content","encrypted_content":"eto-output-ciphertext"}]}
	]`
	second := performCodexNamespaceTestRequest(t, relay, codexSessionRequest(toolResultInput))
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), "tool result accepted") {
		t.Fatalf("tool result response status=%d body=%s", second.Code, second.Body.String())
	}
	plainCapture.mu.Lock()
	plainCallsAfterToolResult := len(plainCapture.bodies)
	plainCapture.mu.Unlock()
	if plainCallsAfterToolResult != 0 {
		t.Fatalf("tool result escaped to switched provider: plain calls=%d", plainCallsAfterToolResult)
	}

	thirdInput := `[
	  {"type":"reasoning","id":"rs_eto","encrypted_content":"eto-ciphertext"},
	  {"type":"custom_tool_call","id":"ctc_eto","call_id":"call_eto","namespace":"collaboration","name":"exec","input":"{}"},
	  {"type":"custom_tool_call_output","id":"out_eto","call_id":"call_eto","output":[{"type":"input_text","text":"tool output"},{"type":"encrypted_content","encrypted_content":"eto-output-ciphertext"}]},
	  {"type":"message","id":"msg_eto","role":"assistant","content":[{"type":"output_text","text":"tool result accepted"}]},
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"continue on plain"}]}
	]`
	third := performCodexNamespaceTestRequest(t, relay, codexSessionRequest(thirdInput))
	if third.Code != http.StatusOK || !strings.Contains(third.Body.String(), "switched safely") {
		t.Fatalf("switch response status=%d body=%s", third.Code, third.Body.String())
	}

	plainCapture.mu.Lock()
	plainBodies := append([][]byte(nil), plainCapture.bodies...)
	plainHeaders := append([]http.Header(nil), plainCapture.headers...)
	plainCapture.mu.Unlock()
	if len(plainBodies) != 1 {
		t.Fatalf("plain upstream calls=%d, want 1", len(plainBodies))
	}
	plainBody := plainBodies[0]
	if bytes.Contains(plainBody, []byte("eto-ciphertext")) || bytes.Contains(plainBody, []byte("eto-output-ciphertext")) {
		t.Fatalf("plain provider received ETO ciphertext: %s", plainBody)
	}
	if gjson.GetBytes(plainBody, `input.#(id=="ctc_eto")`).Exists() || gjson.GetBytes(plainBody, `input.#(id=="msg_eto")`).Exists() {
		t.Fatalf("plain provider received ETO item IDs: %s", plainBody)
	}
	if !gjson.GetBytes(plainBody, `input.#(type=="custom_tool_call_output")`).Exists() ||
		!bytes.Contains(plainBody, []byte("tool output")) || !bytes.Contains(plainBody, []byte("tool result accepted")) {
		t.Fatalf("switch removed portable conversation/tool result: %s", plainBody)
	}
	for _, headerName := range []string{"Session_id", "Thread_id", "X-Client-Request-Id", "X-Codex-Turn-Metadata"} {
		if plainHeaders[0].Get(headerName) != "" {
			t.Fatalf("plain provider received provider-bound header %s", headerName)
		}
	}
}

func TestCodexProviderSessionFallbackCanTakeOverPinnedToolResult(t *testing.T) {
	var primaryCalls, fallbackCalls int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		if primaryCalls == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(codexSessionSSE(`{"type":"function_call","id":"fc_primary","call_id":"call_primary","name":"work","arguments":"{}"}`)))
			return
		}
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer primary.Close()

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls++
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("primary-ciphertext")) {
			t.Fatalf("fallback received primary ciphertext: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codexSessionSSE(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"fallback accepted"}]}`)))
	}))
	defer fallback.Close()

	providers, relay := newTestRelayService(t)
	setNamespaceTestBlacklistEnabled(t, false)
	if err := providers.SaveProviders(ProviderKindCodex, []Provider{
		{ID: 1, Name: "primary", APIURL: primary.URL, APIKey: "key-1", Enabled: true, Level: 1},
		{ID: 2, Name: "fallback", APIURL: fallback.URL, APIKey: "key-2", Enabled: true, Level: 2},
	}); err != nil {
		t.Fatalf("SaveProviders: %v", err)
	}

	first := performCodexNamespaceTestRequest(t, relay, codexSessionRequest(`[{"type":"message","role":"user","content":"start"}]`))
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	secondInput := `[
	  {"type":"reasoning","id":"rs_primary","encrypted_content":"primary-ciphertext"},
	  {"type":"function_call","id":"fc_primary","call_id":"call_primary","name":"work","arguments":"{}"},
	  {"type":"function_call_output","call_id":"call_primary","output":"done"}
	]`
	second := performCodexNamespaceTestRequest(t, relay, codexSessionRequest(secondInput))
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), "fallback accepted") {
		t.Fatalf("fallback status=%d body=%s", second.Code, second.Body.String())
	}
	if primaryCalls != 2 || fallbackCalls != 1 {
		t.Fatalf("calls primary=%d fallback=%d, want 2/1", primaryCalls, fallbackCalls)
	}
}

func TestCodexProviderSessionRetriesExplicitEncryptedHistoryErrorAfterRestart(t *testing.T) {
	var calls int
	var bodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, append([]byte(nil), body...))
		if bytes.Contains(body, []byte("foreign-ciphertext")) {
			http.Error(w, "Encrypted function output content could not be decrypted or decoded", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codexSessionSSE(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recovered"}]}`)))
	}))
	defer upstream.Close()

	providers, relay := newTestRelayService(t)
	if err := providers.SaveProviders(ProviderKindCodex, []Provider{{
		ID: 1, Name: "restart-target", APIURL: upstream.URL, APIKey: "key", Enabled: true,
	}}); err != nil {
		t.Fatalf("SaveProviders: %v", err)
	}
	body := codexSessionRequest(`[
	  {"type":"reasoning","id":"rs_foreign","encrypted_content":"foreign-ciphertext"},
	  {"type":"function_call","id":"fc_foreign","call_id":"call_1","name":"work","arguments":"{}"},
	  {"type":"function_call_output","id":"out_foreign","call_id":"call_1","output":"done"}
	]`)
	recorder := performCodexNamespaceTestRequest(t, relay, body)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "recovered") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if calls != 2 || bytes.Contains(bodies[1], []byte("foreign-ciphertext")) {
		t.Fatalf("calls=%d retry body=%s", calls, bodies[1])
	}
}

func TestCodexProviderSessionTracksCompressedResponseWithoutNamespaceRewrite(t *testing.T) {
	var primaryCalls, fallbackCalls int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		body, _ := io.ReadAll(r.Body)
		output := `{"type":"function_call","id":"fc_gzip","call_id":"call_gzip","name":"work","arguments":"{}"}`
		if gjson.GetBytes(body, `input.#(type=="function_call_output")`).Exists() {
			output = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"gzip tool result accepted"}]}`
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		writer := gzip.NewWriter(w)
		_, _ = writer.Write([]byte(codexSessionSSE(output)))
		_ = writer.Close()
	}))
	defer primary.Close()

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codexSessionSSE(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"wrong provider"}]}`)))
	}))
	defer fallback.Close()

	providers, relay := newTestRelayService(t)
	providerList := []Provider{
		{ID: 1, Name: "gzip-primary", APIURL: primary.URL, APIKey: "key-1", Enabled: true, Level: 1},
		{ID: 2, Name: "fallback", APIURL: fallback.URL, APIKey: "key-2", Enabled: false, Level: 1},
	}
	if err := providers.SaveProviders(ProviderKindCodex, providerList); err != nil {
		t.Fatalf("SaveProviders initial: %v", err)
	}

	first := performCodexNamespaceTestRequestAtPath(
		t,
		relay,
		"/responses",
		codexSessionRequest(`[{"type":"message","role":"user","content":"start"}]`),
		http.Header{"Accept-Encoding": []string{"br"}},
	)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"call_id":"call_gzip"`) {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}

	providerList[0].Enabled = false
	providerList[1].Enabled = true
	if err := providers.SaveProviders(ProviderKindCodex, providerList); err != nil {
		t.Fatalf("SaveProviders switch: %v", err)
	}
	second := performCodexNamespaceTestRequestAtPath(
		t,
		relay,
		"/responses",
		codexSessionRequest(`[
		  {"type":"function_call","id":"fc_gzip","call_id":"call_gzip","name":"work","arguments":"{}"},
		  {"type":"function_call_output","call_id":"call_gzip","output":"done"}
		]`),
		http.Header{"Accept-Encoding": []string{"br"}},
	)
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), "gzip tool result accepted") {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}
	if primaryCalls != 2 || fallbackCalls != 0 {
		t.Fatalf("calls primary=%d fallback=%d, want 2/0", primaryCalls, fallbackCalls)
	}
}

func TestCodexProviderSessionLateOwnerResponseCannotUndoCutover(t *testing.T) {
	key, ok := codexSessionKeyFromRequest(codexSessionRequest(`[]`), nil)
	if !ok {
		t.Fatal("codexSessionKeyFromRequest() did not find prompt_cache_key")
	}
	relay := &ProviderRelayService{
		codexSessionRoutes: map[codexSessionKey]codexSessionRouteState{
			key: {
				providerName: "eto",
				pendingCalls: true,
				pendingCallIDs: map[string]struct{}{
					"call_eto": {},
				},
			},
		},
	}
	request := &codexSessionRequestContext{
		key:            key,
		ownerProvider:  "eto",
		pendingCalls:   true,
		pendingCallIDs: map[string]struct{}{"call_eto": {}},
	}
	success := &codexResponseObservation{jsonPayloads: 1, terminal: true}

	// A fallback finishes first and takes ownership of the session.
	relay.commitCodexSessionAttempt(&codexHistoryAttempt{
		sessionRequest: request,
		providerName:   "plain",
	}, success)

	// The original ETO request completes later. Its stale response must not
	// restore ETO ownership or its pending tool call state.
	lateETO := &codexResponseObservation{
		jsonPayloads: 1,
		terminal:     true,
		toolCallSeen: true,
		toolCallIDs:  map[string]struct{}{"late_call_eto": {}},
	}
	relay.commitCodexSessionAttempt(&codexHistoryAttempt{
		sessionRequest: request,
		providerName:   "eto",
	}, lateETO)

	state := relay.codexSessionRoutes[key]
	if state.providerName != "plain" {
		t.Fatalf("session owner = %q, want plain", state.providerName)
	}
	if _, exists := state.pendingCallIDs["late_call_eto"]; exists {
		t.Fatalf("late ETO response restored stale pending call: %+v", state.pendingCallIDs)
	}
}

func TestCodexProviderSessionLateUnknownOwnerResponseCannotReplaceFirstSuccess(t *testing.T) {
	key, ok := codexSessionKeyFromRequest(codexSessionRequest(`[]`), nil)
	if !ok {
		t.Fatal("codexSessionKeyFromRequest() did not find prompt_cache_key")
	}
	relay := &ProviderRelayService{codexSessionRoutes: make(map[codexSessionKey]codexSessionRouteState)}
	request := &codexSessionRequestContext{key: key}
	success := &codexResponseObservation{jsonPayloads: 1, terminal: true}

	relay.commitCodexSessionAttempt(&codexHistoryAttempt{
		sessionRequest: request,
		providerName:   "plain",
	}, success)
	relay.commitCodexSessionAttempt(&codexHistoryAttempt{
		sessionRequest: request,
		providerName:   "eto",
	}, success)

	if got := relay.codexSessionRoutes[key].providerName; got != "plain" {
		t.Fatalf("session owner = %q, want first successful provider plain", got)
	}
}
