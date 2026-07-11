package services

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
)

type codexInputNamespaceCapture struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (capture *codexInputNamespaceCapture) add(body []byte) int {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.bodies = append(capture.bodies, append([]byte(nil), body...))
	return len(capture.bodies)
}

func (capture *codexInputNamespaceCapture) snapshot() [][]byte {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	result := make([][]byte, len(capture.bodies))
	for index, body := range capture.bodies {
		result[index] = append([]byte(nil), body...)
	}
	return result
}

func codexInputNamespaceRequest(promptCacheKey string, includeEncryptedHistory bool) []byte {
	encryptedHistory := ""
	functionCallID := ""
	functionOutputID := ""
	if includeEncryptedHistory {
		encryptedHistory = `{"type":"reasoning","id":"rs_foreign","encrypted_content":"foreign-ciphertext"},`
		functionCallID = `"id":"fc_1",`
		functionOutputID = `"id":"out_1",`
	}
	return []byte(fmt.Sprintf(`{
  "model":"gpt-5.6-sol",
  "stream":true,
  "prompt_cache_key":%q,
  "tools":[{"type":"namespace","name":"collaboration"}],
  "input":[
    %s
	{"type":"function_call",%s"namespace":"collaboration","name":"spawn_agent","call_id":"call_1","arguments":"{\"namespace\":\"nested-argument\"}"},
	{"type":"function_call_output",%s"call_id":"call_1","output":"done"},
    {"type":"message","namespace":"collaboration","role":"user","content":[{"type":"input_text","text":"continue","namespace":"nested-content"}]}
  ]
}`, promptCacheKey, encryptedHistory, functionCallID, functionOutputID))
}

func writeCodexInputNamespaceError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"unknown_parameter","param":"input[24].namespace","message":"Unknown parameter: 'input[24].namespace'."}}`))
}

func writeCodexInputNamespaceStreamError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(strings.Join([]string{
		"event: response.failed",
		`data: {"type":"response.failed","response":{"status":"failed","error":{"code":"unknown_parameter","param":"input[24].namespace","message":"Unknown parameter: 'input[24].namespace'."}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")))
}

func writeCodexEncryptedHistoryError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`{"error":{"message":"Encrypted function output content could not be decrypted or decoded"}}`))
}

func writeCodexInputNamespaceSuccess(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(codexSessionSSE(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"namespace history accepted"}]}`)))
}

func directCodexInputNamespaceCount(body []byte) int {
	count := 0
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("namespace").Exists() {
			count++
		}
	}
	return count
}

func TestCodexInputNamespaceFallbackRetriesAndCachesProvider(t *testing.T) {
	var capture codexInputNamespaceCapture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capture.add(body)
		if directCodexInputNamespaceCount(body) > 0 {
			writeCodexInputNamespaceStreamError(w)
			return
		}
		writeCodexInputNamespaceSuccess(w)
	}))
	defer upstream.Close()

	providers, relay := newTestRelayService(t)
	if err := providers.SaveProviders(ProviderKindCodex, []Provider{{
		ID: 1, Name: "eto-namespace-reject", APIURL: upstream.URL, APIKey: "key", Enabled: true,
		CodexMultiAgentNamespaceRewrite: true,
	}}); err != nil {
		t.Fatalf("SaveProviders: %v", err)
	}

	for requestIndex := 0; requestIndex < 2; requestIndex++ {
		recorder := performCodexNamespaceTestRequest(t, relay, codexInputNamespaceRequest("namespace-cache-session", false))
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "namespace history accepted") {
			t.Fatalf("request %d status=%d body=%s", requestIndex+1, recorder.Code, recorder.Body.String())
		}
	}

	bodies := capture.snapshot()
	if len(bodies) != 3 {
		t.Fatalf("upstream calls=%d, want 3 (rejection + retry + cached request)", len(bodies))
	}
	if got := directCodexInputNamespaceCount(bodies[0]); got != 2 {
		t.Fatalf("initial direct namespace count=%d, want 2: %s", got, bodies[0])
	}
	for index, body := range bodies[1:] {
		if got := directCodexInputNamespaceCount(body); got != 0 {
			t.Fatalf("sanitized request %d direct namespace count=%d: %s", index+1, got, body)
		}
		if got := gjson.GetBytes(body, "tools.0.name").String(); got != "agents" {
			t.Fatalf("sanitized request %d tool namespace=%q", index+1, got)
		}
		if got := gjson.GetBytes(body, "input.0.arguments").String(); got != `{"namespace":"nested-argument"}` {
			t.Fatalf("sanitized request %d arguments changed to %q", index+1, got)
		}
		if got := gjson.GetBytes(body, "input.2.content.0.namespace").String(); got != "nested-content" {
			t.Fatalf("sanitized request %d nested namespace changed to %q", index+1, got)
		}
	}
}

func TestCodexInputNamespaceCompatibleStreamPassesThroughUnchanged(t *testing.T) {
	var capture codexInputNamespaceCapture
	upstreamBody := strings.Join([]string{
		": keepalive",
		"event: response.created",
		`data: {"type":"response.created","response":{"status":"in_progress","output":[]}}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"compatible"}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"compatible"}]}],"usage":{"input_tokens":7,"output_tokens":1}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capture.add(body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	providers, relay := newTestRelayService(t)
	if err := providers.SaveProviders(ProviderKindCodex, []Provider{{
		ID: 1, Name: "namespace-compatible", APIURL: upstream.URL, APIKey: "key", Enabled: true,
		CodexMultiAgentNamespaceRewrite: true,
	}}); err != nil {
		t.Fatalf("SaveProviders: %v", err)
	}

	recorder := performCodexNamespaceTestRequest(t, relay, codexInputNamespaceRequest("namespace-compatible", false))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != upstreamBody {
		t.Fatalf("compatible stream changed:\n got: %q\nwant: %q", recorder.Body.String(), upstreamBody)
	}
	bodies := capture.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("upstream calls=%d, want 1", len(bodies))
	}
	if got := directCodexInputNamespaceCount(bodies[0]); got != 2 {
		t.Fatalf("compatible provider request namespace count=%d, want 2: %s", got, bodies[0])
	}
}

func TestCodexInputNamespaceFallbackCombinesWithEncryptedHistory(t *testing.T) {
	for _, namespaceFirst := range []bool{true, false} {
		name := "encrypted history first"
		if namespaceFirst {
			name = "input namespace first"
		}
		t.Run(name, func(t *testing.T) {
			var capture codexInputNamespaceCapture
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				capture.add(body)
				hasNamespace := directCodexInputNamespaceCount(body) > 0
				hasEncrypted := bytes.Contains(body, []byte("foreign-ciphertext"))
				if namespaceFirst && hasNamespace {
					writeCodexInputNamespaceError(w)
					return
				}
				if hasEncrypted {
					writeCodexEncryptedHistoryError(w)
					return
				}
				if hasNamespace {
					writeCodexInputNamespaceError(w)
					return
				}
				writeCodexInputNamespaceSuccess(w)
			}))
			defer upstream.Close()

			providers, relay := newTestRelayService(t)
			providerName := strings.ReplaceAll(name, " ", "-")
			if err := providers.SaveProviders(ProviderKindCodex, []Provider{{
				ID: 1, Name: providerName, APIURL: upstream.URL, APIKey: "key", Enabled: true,
				CodexMultiAgentNamespaceRewrite: true,
			}}); err != nil {
				t.Fatalf("SaveProviders: %v", err)
			}

			recorder := performCodexNamespaceTestRequest(t, relay, codexInputNamespaceRequest(providerName, true))
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "namespace history accepted") {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			bodies := capture.snapshot()
			if len(bodies) != 3 {
				t.Fatalf("upstream calls=%d, want 3", len(bodies))
			}
			lastBody := bodies[len(bodies)-1]
			if directCodexInputNamespaceCount(lastBody) != 0 || bytes.Contains(lastBody, []byte("foreign-ciphertext")) {
				t.Fatalf("final retry retained incompatible state: %s", lastBody)
			}
			if got := gjson.GetBytes(lastBody, "tools.0.name").String(); got != "agents" {
				t.Fatalf("final retry tool namespace=%q", got)
			}
		})
	}
}

func TestCodexInputNamespaceFallbackDoesNotPolluteProviderFailover(t *testing.T) {
	requestBody := codexInputNamespaceRequest("namespace-failover", false)
	var firstCapture codexInputNamespaceCapture
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := firstCapture.add(body)
		if call == 1 {
			writeCodexInputNamespaceError(w)
			return
		}
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer first.Close()

	var fallbackBody []byte
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackBody, _ = io.ReadAll(r.Body)
		writeCodexInputNamespaceSuccess(w)
	}))
	defer second.Close()

	providers, relay := newTestRelayService(t)
	setNamespaceTestBlacklistEnabled(t, false)
	if err := providers.SaveProviders(ProviderKindCodex, []Provider{
		{ID: 1, Name: "eto-rejects-namespace", APIURL: first.URL, APIKey: "key-1", Enabled: true, Level: 1, CodexMultiAgentNamespaceRewrite: true},
		{ID: 2, Name: "plain-fallback", APIURL: second.URL, APIKey: "key-2", Enabled: true, Level: 2},
	}); err != nil {
		t.Fatalf("SaveProviders: %v", err)
	}

	recorder := performCodexNamespaceTestRequest(t, relay, requestBody)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "namespace history accepted") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(firstCapture.snapshot()) != 2 {
		t.Fatalf("rewrite provider calls=%d, want 2", len(firstCapture.snapshot()))
	}
	if !bytes.Equal(fallbackBody, requestBody) {
		t.Fatalf("fallback received modified request:\n got: %s\nwant: %s", fallbackBody, requestBody)
	}
}
