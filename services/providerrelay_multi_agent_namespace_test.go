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

	"github.com/daodao97/xgo/xdb"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func performCodexNamespaceTestRequest(t *testing.T, relay *ProviderRelayService, body []byte) *httptest.ResponseRecorder {
	return performCodexNamespaceTestRequestAtPath(t, relay, "/responses", body)
}

func performCodexNamespaceTestRequestAtPath(t *testing.T, relay *ProviderRelayService, path string, body []byte, extraHeaders ...http.Header) *httptest.ResponseRecorder {
	t.Helper()
	relayKey, err := relay.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatalf("EnsureDefaultKey: %v", err)
	}
	router := gin.New()
	relay.registerRoutes(router)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+relayKey.Key)
	for _, headers := range extraHeaders {
		for key, values := range headers {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

func setNamespaceTestBlacklistEnabled(t *testing.T, enabled bool) {
	t.Helper()
	db, err := xdb.DB("default")
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	value := "false"
	if enabled {
		value = "true"
	}
	if _, err := db.Exec(`
		INSERT INTO app_settings (key, value) VALUES ('enable_blacklist', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, value); err != nil {
		t.Fatalf("set test blacklist state: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO app_settings (key, value) VALUES ('blacklist_level_enabled', 'false')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`); err != nil {
		t.Fatalf("disable test level blacklist: %v", err)
	}
}

func TestCodexMultiAgentNamespaceRewriteNonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	requestBody := []byte(`{"model":"gpt-5-codex","tools":[{"type":"namespace","name":"collaboration"}],"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"collaboration"}]},{"type":"function_call","namespace":"collaboration","name":"spawn_agent","arguments":"{}"}]}`)
	responseBody := []byte(`{"id":"resp_1","output":[{"type":"function_call","namespace":"agents","name":"spawn_agent","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(responseBody)
	}))
	defer upstream.Close()

	providers, relay := newTestRelayService(t)
	if err := providers.SaveProviders(ProviderKindCodex, []Provider{{
		ID:                              1,
		Name:                            "namespace-rewrite",
		APIURL:                          upstream.URL,
		APIKey:                          "upstream-key",
		Enabled:                         true,
		Level:                           1,
		CodexMultiAgentNamespaceRewrite: true,
	}}); err != nil {
		t.Fatalf("SaveProviders: %v", err)
	}

	recorder := performCodexNamespaceTestRequest(t, relay, requestBody)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := gjson.GetBytes(upstreamBody, "tools.0.name").String(); got != "agents" {
		t.Fatalf("upstream tool namespace = %q, body = %s", got, upstreamBody)
	}
	if got := gjson.GetBytes(upstreamBody, "input.0.tools.0.name").String(); got != "agents" {
		t.Fatalf("upstream additional_tools namespace = %q, body = %s", got, upstreamBody)
	}
	if got := gjson.GetBytes(upstreamBody, "input.1.namespace").String(); got != "agents" {
		t.Fatalf("upstream history namespace = %q, body = %s", got, upstreamBody)
	}
	if got := gjson.Get(recorder.Body.String(), "output.0.namespace").String(); got != "collaboration" {
		t.Fatalf("client response namespace = %q, body = %s", got, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Length"); got != "" {
		t.Fatalf("rewritten response retained stale Content-Length %q", got)
	}
}

func TestCodexMultiAgentNamespaceRewriteRequiresForwardModification(t *testing.T) {
	tests := []struct {
		name        string
		enabled     bool
		path        string
		requestBody []byte
	}{
		{
			name:        "feature disabled",
			enabled:     false,
			path:        "/responses",
			requestBody: []byte(`{"model":"gpt-5-codex","tools":[{"type":"namespace","name":"collaboration"}]}`),
		},
		{
			name:        "client already uses agents",
			enabled:     true,
			path:        "/responses",
			requestBody: []byte(`{"model":"gpt-5-codex","tools":[{"type":"namespace","name":"agents"}]}`),
		},
		{
			name:        "non responses endpoint",
			enabled:     true,
			path:        "/v1/chat/completions",
			requestBody: []byte(`{"model":"gpt-5-codex","tools":[{"type":"namespace","name":"collaboration"}]}`),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var upstreamBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"output":[{"type":"function_call","namespace":"agents","name":"spawn_agent"}]}`))
			}))
			defer upstream.Close()

			providers, relay := newTestRelayService(t)
			if err := providers.SaveProviders(ProviderKindCodex, []Provider{{
				ID:                              1,
				Name:                            "namespace-noop",
				APIURL:                          upstream.URL,
				APIKey:                          "upstream-key",
				Enabled:                         true,
				CodexMultiAgentNamespaceRewrite: test.enabled,
			}}); err != nil {
				t.Fatalf("SaveProviders: %v", err)
			}

			recorder := performCodexNamespaceTestRequestAtPath(t, relay, test.path, test.requestBody)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if !bytes.Equal(upstreamBody, test.requestBody) {
				t.Fatalf("no-op request changed:\n got: %s\nwant: %s", upstreamBody, test.requestBody)
			}
			if got := gjson.Get(recorder.Body.String(), "output.0.namespace").String(); got != "agents" {
				t.Fatalf("response was rewritten without a forward rewrite: %q", got)
			}
		})
	}
}

func TestCodexMultiAgentNamespaceRewriteStreaming(t *testing.T) {
	upstreamSSE := strings.Join([]string{
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","item":{"type":"function_call","namespace":"agents","name":"spawn_agent"}}`,
		"",
		"event: response.output_item.done",
		`data: {"type":"response.output_item.done","item":{"type":"custom_tool_call","namespace":"agents","name":"send_message"}}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"output":[{"type":"function_call","namespace":"agents","name":"spawn_agent"}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept-Encoding"); !strings.Contains(got, "gzip") {
			t.Errorf("relay did not let net/http negotiate gzip, Accept-Encoding = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		writer := gzip.NewWriter(w)
		_, _ = writer.Write([]byte(upstreamSSE))
		_ = writer.Close()
	}))
	defer upstream.Close()

	providers, relay := newTestRelayService(t)
	if err := providers.SaveProviders(ProviderKindCodex, []Provider{{
		ID:                              1,
		Name:                            "namespace-stream",
		APIURL:                          upstream.URL,
		APIKey:                          "upstream-key",
		Enabled:                         true,
		CodexMultiAgentNamespaceRewrite: true,
	}}); err != nil {
		t.Fatalf("SaveProviders: %v", err)
	}
	requestBody := []byte(`{"model":"gpt-5-codex","stream":true,"tools":[{"type":"namespace","name":"collaboration"}]}`)
	recorder := performCodexNamespaceTestRequestAtPath(t, relay, "/responses", requestBody, http.Header{"Accept-Encoding": []string{"br"}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	response := recorder.Body.String()
	if strings.Contains(response, `"namespace":"agents"`) {
		t.Fatalf("stream retained upstream namespace: %s", response)
	}
	if count := strings.Count(response, `"namespace":"collaboration"`); count != 3 {
		t.Fatalf("rewritten namespace count = %d, response = %s", count, response)
	}
	for _, unchanged := range []string{"event: response.output_item.added", "event: response.output_item.done", "event: response.completed", "data: [DONE]"} {
		if !strings.Contains(response, unchanged) {
			t.Errorf("stream missing unchanged framing %q: %s", unchanged, response)
		}
	}
	if got := recorder.Header().Get("Content-Length"); got != "" {
		t.Fatalf("stream retained stale Content-Length %q", got)
	}
	if got := recorder.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("rewritten stream retained Content-Encoding %q", got)
	}
}

func TestCodexMultiAgentNamespaceStreamRequestWithJSONResponse(t *testing.T) {
	for _, test := range []struct {
		name        string
		contentType string
	}{
		{name: "json content type", contentType: "application/json"},
		{name: "missing content type"},
		{name: "incorrect SSE content type", contentType: "text/event-stream"},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.contentType != "" {
					w.Header().Set("Content-Type", test.contentType)
				}
				_, _ = w.Write([]byte(`{"output":[{"type":"function_call","namespace":"agents","name":"spawn_agent"}]}`))
			}))
			defer upstream.Close()

			providers, relay := newTestRelayService(t)
			if err := providers.SaveProviders(ProviderKindCodex, []Provider{{
				ID:                              1,
				Name:                            "namespace-json-fallback",
				APIURL:                          upstream.URL,
				APIKey:                          "upstream-key",
				Enabled:                         true,
				CodexMultiAgentNamespaceRewrite: true,
			}}); err != nil {
				t.Fatalf("SaveProviders: %v", err)
			}
			requestBody := []byte(`{"model":"gpt-5-codex","stream":true,"tools":[{"type":"namespace","name":"collaboration"}]}`)
			recorder := performCodexNamespaceTestRequest(t, relay, requestBody)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if got := gjson.Get(recorder.Body.String(), "output.0.namespace").String(); got != "collaboration" {
				t.Fatalf("JSON fallback namespace = %q, body = %s", got, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
				t.Fatalf("JSON fallback content type = %q", got)
			}
		})
	}
}

func TestCodexMultiAgentNamespaceFailoverUsesOriginalRequest(t *testing.T) {
	requestBody := []byte(`{"model":"gpt-5-codex","tools":[{"type":"namespace","name":"collaboration"}],"input":[{"type":"function_call","namespace":"collaboration","name":"spawn_agent"}]}`)
	var mu sync.Mutex
	var rewrittenBodies [][]byte
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		rewrittenBodies = append(rewrittenBodies, append([]byte(nil), body...))
		mu.Unlock()
		http.Error(w, "upstream failed", http.StatusInternalServerError)
	}))
	defer first.Close()
	var fallbackBody []byte
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed"}`))
	}))
	defer second.Close()

	providers, relay := newTestRelayService(t)
	setNamespaceTestBlacklistEnabled(t, false)
	if err := providers.SaveProviders(ProviderKindCodex, []Provider{
		{ID: 1, Name: "rewrite-fails", APIURL: first.URL, APIKey: "key-1", Enabled: true, Level: 1, CodexMultiAgentNamespaceRewrite: true},
		{ID: 2, Name: "plain-fallback", APIURL: second.URL, APIKey: "key-2", Enabled: true, Level: 2},
	}); err != nil {
		t.Fatalf("SaveProviders: %v", err)
	}

	recorder := performCodexNamespaceTestRequest(t, relay, requestBody)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	mu.Lock()
	firstBodies := append([][]byte(nil), rewrittenBodies...)
	mu.Unlock()
	if len(firstBodies) == 0 {
		t.Fatal("rewrite provider was not attempted")
	}
	for _, body := range firstBodies {
		if gjson.GetBytes(body, "tools.0.name").String() != "agents" || gjson.GetBytes(body, "input.0.namespace").String() != "agents" {
			t.Fatalf("rewrite provider received unconverted body: %s", body)
		}
	}
	if !bytes.Equal(fallbackBody, requestBody) {
		t.Fatalf("fallback provider received polluted body:\n got: %s\nwant: %s", fallbackBody, requestBody)
	}
}

func TestCodexMultiAgentNamespaceConflictSkipsRewriteProvider(t *testing.T) {
	conflictingBody := []byte(`{"model":"gpt-5-codex","tools":[{"type":"namespace","name":"collaboration"}],"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"agents"}]}]}`)
	conflictHits := 0
	conflictUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conflictHits++
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer conflictUpstream.Close()
	var fallbackBody []byte
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed"}`))
	}))
	defer fallback.Close()

	providers, relay := newTestRelayService(t)
	setNamespaceTestBlacklistEnabled(t, true)
	t.Cleanup(func() { setNamespaceTestBlacklistEnabled(t, false) })
	if err := providers.SaveProviders(ProviderKindCodex, []Provider{
		{ID: 1, Name: "conflicting-rewrite-provider", APIURL: conflictUpstream.URL, APIKey: "key-1", Enabled: true, Level: 1, CodexMultiAgentNamespaceRewrite: true},
		{ID: 2, Name: "plain-conflict-fallback", APIURL: fallback.URL, APIKey: "key-2", Enabled: true, Level: 2},
	}); err != nil {
		t.Fatalf("SaveProviders: %v", err)
	}

	recorder := performCodexNamespaceTestRequest(t, relay, conflictingBody)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if conflictHits != 0 {
		t.Fatalf("conflicting rewrite provider hit count = %d, want 0", conflictHits)
	}
	if !bytes.Equal(fallbackBody, conflictingBody) {
		t.Fatalf("plain provider did not receive original conflict request:\n got: %s\nwant: %s", fallbackBody, conflictingBody)
	}
	statuses, err := relay.blacklistService.GetBlacklistStatus(ProviderKindCodex)
	if err != nil {
		t.Fatalf("GetBlacklistStatus: %v", err)
	}
	for _, status := range statuses {
		if status.ProviderName == "conflicting-rewrite-provider" && status.FailureCount != 0 {
			t.Fatalf("conflicting provider was counted as failed: %+v", status)
		}
	}
}
