package services

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type degradationNamespaceRequestCapture struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (capture *degradationNamespaceRequestCapture) add(body []byte) int {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.bodies = append(capture.bodies, append([]byte(nil), body...))
	return len(capture.bodies)
}

func (capture *degradationNamespaceRequestCapture) snapshot() [][]byte {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	result := make([][]byte, len(capture.bodies))
	for i, body := range capture.bodies {
		result[i] = append([]byte(nil), body...)
	}
	return result
}

func enableDegradationForNamespaceHandlerTest(t *testing.T, relay *ProviderRelayService) {
	t.Helper()
	original, err := relay.appSettings.GetAppSettings()
	if err != nil {
		t.Fatalf("GetAppSettings: %v", err)
	}
	updated := original
	updated.CodexDegradationResendEnabled = true
	updated.CodexDegradationMaxResend = 1
	updated.CodexDegradationReasoningTokens = []int{516}
	if _, err := relay.appSettings.SaveAppSettings(updated); err != nil {
		t.Fatalf("SaveAppSettings: %v", err)
	}
	t.Cleanup(func() {
		if _, err := relay.appSettings.SaveAppSettings(original); err != nil {
			t.Errorf("restore AppSettings: %v", err)
		}
	})
}

func saveDegradationNamespaceProvider(t *testing.T, providers *ProviderService, name, upstreamURL string) {
	t.Helper()
	if err := providers.SaveProviders(ProviderKindCodex, []Provider{{
		ID:                              1,
		Name:                            name,
		APIURL:                          upstreamURL,
		APIKey:                          "upstream-key",
		Enabled:                         true,
		Level:                           1,
		CodexMultiAgentNamespaceRewrite: true,
	}}); err != nil {
		t.Fatalf("SaveProviders: %v", err)
	}
}

func performDegradationNamespaceHandlerRequest(t *testing.T, relay *ProviderRelayService, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	relayKey, err := relay.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatalf("EnsureDefaultKey: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)
	req := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+relayKey.Key)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

func degradationNamespaceSSE(reasoningTokens int) []byte {
	return []byte(fmt.Sprintf(
		"event: response.output_item.added\n"+
			"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"namespace\":\"agents\",\"name\":\"spawn_agent\"}}\n\n"+
			"event: response.output_text.delta\n"+
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"+
			"event: response.completed\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"function_call\",\"namespace\":\"agents\",\"name\":\"spawn_agent\"}],\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":3},\"output_tokens_details\":{\"reasoning_tokens\":%d}}}}\n\n"+
			"data: [DONE]\n\n",
		reasoningTokens,
	))
}

func TestDegradationNamespaceHandlerStreamingRewriteAndRetry(t *testing.T) {
	var capture degradationNamespaceRequestCapture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		attempt := capture.add(body)
		reasoning := 516
		if attempt > 1 {
			reasoning = 800
		}
		responseBody := degradationNamespaceSSE(reasoning)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(responseBody)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(responseBody)
	}))
	defer upstream.Close()

	providers, relay := newTestRelayService(t)
	enableDegradationForNamespaceHandlerTest(t, relay)
	saveDegradationNamespaceProvider(t, providers, "degradation-namespace-stream", upstream.URL)
	requestBody := []byte(`{"model":"gpt-5-codex","stream":true,"tools":[{"type":"namespace","name":"collaboration"}]}`)

	recorder := performDegradationNamespaceHandlerRequest(t, relay, requestBody)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	bodies := capture.snapshot()
	if len(bodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(bodies))
	}
	for i, body := range bodies {
		if got := gjson.GetBytes(body, "tools.0.name").String(); got != "agents" {
			t.Fatalf("attempt %d upstream namespace = %q, body = %s", i+1, got, body)
		}
	}

	response := recorder.Body.String()
	if strings.Contains(response, `"namespace":"agents"`) {
		t.Fatalf("client stream retained agents namespace: %s", response)
	}
	if count := strings.Count(response, `"namespace":"collaboration"`); count != 2 {
		t.Fatalf("client collaboration namespace count = %d, response = %s", count, response)
	}
	if strings.Contains(response, `"reasoning_tokens":516`) || !strings.Contains(response, `"reasoning_tokens":800`) {
		t.Fatalf("client did not receive only the final non-degraded response: %s", response)
	}
	if !strings.Contains(response, "data: [DONE]") {
		t.Fatalf("client stream lost DONE sentinel: %s", response)
	}
	if got := recorder.Header().Get("Content-Length"); got != "" {
		t.Fatalf("rewritten buffered stream retained stale Content-Length %q", got)
	}
}

func TestDegradationNamespaceHandlerStreamingMislabeledAsJSON(t *testing.T) {
	var capture degradationNamespaceRequestCapture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		attempt := capture.add(body)
		reasoning := 516
		if attempt > 1 {
			reasoning = 800
		}
		responseBody := degradationNamespaceSSE(reasoning)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(responseBody)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(responseBody)
	}))
	defer upstream.Close()

	providers, relay := newTestRelayService(t)
	enableDegradationForNamespaceHandlerTest(t, relay)
	saveDegradationNamespaceProvider(t, providers, "degradation-namespace-mislabeled-stream", upstream.URL)
	requestBody := []byte("{\"model\":\"gpt-5-codex\",\"stream\":true,\"tools\":[{\"type\":\"namespace\",\"name\":\"collaboration\"}]}")

	recorder := performDegradationNamespaceHandlerRequest(t, relay, requestBody)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	bodies := capture.snapshot()
	if len(bodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2; mislabeled SSE usage was not parsed", len(bodies))
	}
	for i, body := range bodies {
		if got := gjson.GetBytes(body, "tools.0.name").String(); got != "agents" {
			t.Fatalf("attempt %d upstream namespace = %q, body = %s", i+1, got, body)
		}
	}

	response := recorder.Body.String()
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("mislabeled stream content type was not normalized: %q", got)
	}
	if strings.Contains(response, "\"namespace\":\"agents\"") {
		t.Fatalf("client stream retained agents namespace: %s", response)
	}
	for _, expected := range []string{"\"namespace\":\"collaboration\"", "\"delta\":\"hello\"", "\"cached_tokens\":3", "\"reasoning_tokens\":800", "data: [DONE]"} {
		if !strings.Contains(response, expected) {
			t.Errorf("client stream missing %q: %s", expected, response)
		}
	}
	if strings.Contains(response, "\"reasoning_tokens\":516") {
		t.Fatalf("client received the discarded degraded response: %s", response)
	}
	if got := recorder.Header().Get("Content-Length"); got != "" {
		t.Fatalf("rewritten buffered stream retained stale Content-Length %q", got)
	}
}

func TestDegradationNamespaceHandlerRetriesWithoutForeignProviderHistory(t *testing.T) {
	var capture degradationNamespaceRequestCapture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		attempt := capture.add(body)
		w.Header().Set("Content-Type", "text/event-stream")
		if gjson.GetBytes(body, `input.#(id=="rs_foreign")`).Exists() {
			_, _ = w.Write([]byte(providerHistoryTestSSE(true)))
			return
		}
		reasoning := 516
		if attempt > 2 {
			reasoning = 800
		}
		_, _ = w.Write(degradationNamespaceSSE(reasoning))
	}))
	defer upstream.Close()

	providers, relay := newTestRelayService(t)
	enableDegradationForNamespaceHandlerTest(t, relay)
	saveDegradationNamespaceProvider(t, providers, "degradation-provider-history-fallback", upstream.URL)

	recorder := performDegradationNamespaceHandlerRequest(t, relay, codexHistoryFallbackRequestBody())
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	bodies := capture.snapshot()
	if len(bodies) != 3 {
		t.Fatalf("upstream calls = %d, want 3 (empty response + sanitized retry + degradation resend)", len(bodies))
	}
	if !gjson.GetBytes(bodies[0], `input.#(id=="rs_foreign")`).Exists() {
		t.Fatalf("initial request unexpectedly removed foreign reasoning: %s", bodies[0])
	}
	for index, body := range bodies[1:] {
		if gjson.GetBytes(body, `input.#(id=="rs_foreign")`).Exists() {
			t.Fatalf("sanitized request %d retained foreign reasoning: %s", index+1, body)
		}
		if got := gjson.GetBytes(body, "tools.0.name").String(); got != "agents" {
			t.Fatalf("sanitized request %d namespace = %q, body = %s", index+1, got, body)
		}
	}

	response := recorder.Body.String()
	if strings.Contains(response, `"namespace":"agents"`) || strings.Contains(response, `"reasoning_tokens":516`) {
		t.Fatalf("client received an intermediate response: %s", response)
	}
	for _, expected := range []string{`"namespace":"collaboration"`, `"delta":"hello"`, `"reasoning_tokens":800`, "data: [DONE]"} {
		if !strings.Contains(response, expected) {
			t.Errorf("client response missing %q: %s", expected, response)
		}
	}
}

func TestDegradationNamespaceHandlerClientNativeAgentsDoesNotReverse(t *testing.T) {
	var capture degradationNamespaceRequestCapture
	responseBody := []byte(`{"output":[{"type":"function_call","namespace":"agents","name":"spawn_agent"}],"usage":{"input_tokens":1,"output_tokens":1,"output_tokens_details":{"reasoning_tokens":800}}}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capture.add(body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(responseBody)))
		_, _ = w.Write(responseBody)
	}))
	defer upstream.Close()

	providers, relay := newTestRelayService(t)
	enableDegradationForNamespaceHandlerTest(t, relay)
	saveDegradationNamespaceProvider(t, providers, "degradation-native-agents", upstream.URL)
	requestBody := []byte(`{"model":"gpt-5-codex","tools":[{"type":"namespace","name":"agents"}]}`)

	recorder := performDegradationNamespaceHandlerRequest(t, relay, requestBody)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	bodies := capture.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(bodies))
	}
	if !bytes.Equal(bodies[0], requestBody) {
		t.Fatalf("native agents request changed:\n got: %s\nwant: %s", bodies[0], requestBody)
	}
	if got := gjson.Get(recorder.Body.String(), "output.0.namespace").String(); got != "agents" {
		t.Fatalf("native agents response namespace = %q, body = %s", got, recorder.Body.String())
	}
}

func TestDegradationNamespaceHandlerStreamRequestJSONFallback(t *testing.T) {
	var capture degradationNamespaceRequestCapture
	responseBody := []byte(`{"output":[{"type":"custom_tool_call","namespace":"agents","name":"send_message"}],"usage":{"input_tokens":1,"output_tokens":1,"output_tokens_details":{"reasoning_tokens":800}}}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capture.add(body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(responseBody)))
		_, _ = w.Write(responseBody)
	}))
	defer upstream.Close()

	providers, relay := newTestRelayService(t)
	enableDegradationForNamespaceHandlerTest(t, relay)
	saveDegradationNamespaceProvider(t, providers, "degradation-namespace-json-fallback", upstream.URL)
	requestBody := []byte(`{"model":"gpt-5-codex","stream":true,"tools":[{"type":"namespace","name":"collaboration"}]}`)

	recorder := performDegradationNamespaceHandlerRequest(t, relay, requestBody)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	bodies := capture.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(bodies))
	}
	if got := gjson.GetBytes(bodies[0], "tools.0.name").String(); got != "agents" {
		t.Fatalf("upstream namespace = %q, body = %s", got, bodies[0])
	}
	if got := gjson.Get(recorder.Body.String(), "output.0.namespace").String(); got != "collaboration" {
		t.Fatalf("JSON fallback namespace = %q, body = %s", got, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Length"); got != "" {
		t.Fatalf("rewritten JSON fallback retained stale Content-Length %q", got)
	}
}
