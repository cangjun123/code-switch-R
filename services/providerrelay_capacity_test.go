package services

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

func codexCapacitySSE(code, message string) string {
	return strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"status":"in_progress","output":[]}}`,
		"",
		"event: response.failed",
		`data: {"type":"response.failed","response":{"status":"failed","error":{"code":"` + code + `","message":"` + message + `"}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
}

func codexCapacityAfterOutputSSE(code string) string {
	return strings.Join([]string{
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		"",
		"event: response.failed",
		`data: {"type":"response.failed","response":{"status":"failed","error":{"code":"` + code + `","message":"try another model"}}}`,
		"",
	}, "\n")
}

func setCapacityTestDegradation(t *testing.T, relay *ProviderRelayService, enabled bool) {
	t.Helper()
	settings, err := relay.appSettings.GetAppSettings()
	if err != nil {
		t.Fatalf("GetAppSettings: %v", err)
	}
	previous := settings
	settings.CodexDegradationResendEnabled = enabled
	settings.EnableRoundRobin = false
	if _, err := relay.appSettings.SaveAppSettings(settings); err != nil {
		t.Fatalf("SaveAppSettings: %v", err)
	}
	t.Cleanup(func() {
		if _, err := relay.appSettings.SaveAppSettings(previous); err != nil {
			t.Errorf("restore app settings: %v", err)
		}
	})
}

func TestCodexCapacityErrorFromPayload(t *testing.T) {
	tests := []struct {
		name        string
		payload     string
		wantCode    string
		wantMessage string
	}{
		{
			name:        "nested server overloaded",
			payload:     `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_is_overloaded","message":"model busy"}}}`,
			wantCode:    "server_is_overloaded",
			wantMessage: "model busy",
		},
		{
			name:        "top level slow down",
			payload:     `{"type":"error","error":{"code":"slow_down","message":"retry later"}}`,
			wantCode:    "slow_down",
			wantMessage: "retry later",
		},
		{
			name:    "rate limit is out of scope",
			payload: `{"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"retry"}}}`,
		},
		{
			name:    "visible message without code is ignored",
			payload: `{"type":"response.failed","response":{"status":"failed","error":{"message":"Selected model is at capacity. Please try a different model."}}}`,
		},
		{
			name:    "capacity code on successful response is ignored",
			payload: `{"type":"response.completed","response":{"status":"completed","error":{"code":"server_is_overloaded"}}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := codexCapacityErrorFromPayload([]byte(test.payload))
			if test.wantCode == "" {
				if got != nil {
					t.Fatalf("unexpected capacity error: %#v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("capacity error was not detected")
			}
			if got.Code != test.wantCode || got.Message != test.wantMessage {
				t.Fatalf("capacity error = %#v, want code=%q message=%q", got, test.wantCode, test.wantMessage)
			}
		})
	}
}

func TestCodexCapacityErrorFromResponseBody(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		code string
	}{
		{
			name: "SSE final line without newline",
			body: `event: response.failed
data: {"type":"response.failed","response":{"status":"failed","error":{"code":"server_is_overloaded"}}}`,
			code: "server_is_overloaded",
		},
		{
			name: "non-streaming JSON",
			body: `{"status":"failed","error":{"code":"slow_down","message":"later"}}`,
			code: "slow_down",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := codexCapacityErrorFromResponseBody([]byte(test.body))
			if got == nil || got.Code != test.code {
				t.Fatalf("capacity error = %#v, want code %q", got, test.code)
			}
		})
	}
}

func TestCodexProviderHistoryDoesNotRetryCapacityFailure(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_is_overloaded"}}}`)
	terminal, retry, reason := codexPayloadProviderHistoryDecision(payload)
	if !terminal || retry || reason != "capacity_failure" {
		t.Fatalf("history decision = terminal:%v retry:%v reason:%q", terminal, retry, reason)
	}
}

func TestPostCodexResponsesCapacityPreflightDetectsNonStreamingJSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"failed","error":{"code":"slow_down","message":"retry later"}}`))
	}))
	defer upstream.Close()

	relay := &ProviderRelayService{httpClient: newRelayHTTPClient()}
	resp, err := relay.postCodexResponsesRequestWithCapacityPreflight(
		context.Background(),
		upstream.URL,
		nil,
		map[string]string{"Content-Type": "application/json"},
		[]byte(`{"model":"gpt-5-codex","stream":false}`),
		false,
		"capacity-json",
		nil,
	)
	if resp != nil {
		t.Fatalf("capacity response was returned for passthrough: %#v", resp)
	}
	var capacityErr *codexProviderCapacityError
	if !errors.As(err, &capacityErr) {
		t.Fatalf("error = %v, want codexProviderCapacityError", err)
	}
	if capacityErr.Code != "slow_down" || capacityErr.StatusCode != http.StatusOK {
		t.Fatalf("capacity error = %#v", capacityErr)
	}
}

func TestPostCodexResponsesCapacityPreflightPassesOtherFailuresByteExact(t *testing.T) {
	body := codexCapacitySSE("context_length_exceeded", "input too long")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	relay := &ProviderRelayService{httpClient: newRelayHTTPClient()}
	resp, err := relay.postCodexResponsesRequestWithCapacityPreflight(
		context.Background(),
		upstream.URL,
		nil,
		map[string]string{"Content-Type": "application/json"},
		[]byte(`{"model":"gpt-5-codex","stream":true}`),
		true,
		"non-capacity-failure",
		nil,
	)
	if err != nil {
		t.Fatalf("preflight returned error: %v", err)
	}
	got, readErr := io.ReadAll(resp.RawResponse.Body)
	if readErr != nil {
		t.Fatalf("read replayed response: %v", readErr)
	}
	if string(got) != body {
		t.Fatalf("response changed:\n got: %q\nwant: %q", got, body)
	}
}

func TestPostCodexResponsesCapacityPreflightStopsAtFirstOutput(t *testing.T) {
	body := codexCapacityAfterOutputSSE("server_is_overloaded")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	relay := &ProviderRelayService{httpClient: newRelayHTTPClient()}
	resp, err := relay.postCodexResponsesRequestWithCapacityPreflight(
		context.Background(),
		upstream.URL,
		nil,
		map[string]string{"Content-Type": "application/json"},
		[]byte(`{"model":"gpt-5-codex","stream":true}`),
		true,
		"capacity-after-output",
		nil,
	)
	if err != nil {
		t.Fatalf("post-output capacity must remain passthrough: %v", err)
	}
	got, readErr := io.ReadAll(resp.RawResponse.Body)
	if readErr != nil {
		t.Fatalf("read replayed response: %v", readErr)
	}
	if string(got) != body {
		t.Fatalf("response changed:\n got: %q\nwant: %q", got, body)
	}
}

func TestCodexCapacityResponseFallsBackToNextProvider(t *testing.T) {
	var capacityCalls atomic.Int32
	capacity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		capacityCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codexCapacitySSE("server_is_overloaded", "model busy")))
	}))
	defer capacity.Close()

	successBody := codexSessionSSE(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"fallback success"}]}`)
	var successCalls atomic.Int32
	success := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		successCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(successBody))
	}))
	defer success.Close()

	providers, relay := newTestRelayService(t)
	setNamespaceRoutingDBSetting(t, "enable_blacklist", "false")
	setNamespaceRoutingDBSetting(t, "blacklist_level_enabled", "false")
	setCapacityTestDegradation(t, relay, false)
	if err := providers.SaveProviders(ProviderKindCodex, []Provider{
		{ID: 1, Name: "capacity-fallback-primary", APIURL: capacity.URL, APIKey: "key-1", Enabled: true, Level: 1},
		{ID: 2, Name: "capacity-fallback-secondary", APIURL: success.URL, APIKey: "key-2", Enabled: true, Level: 2},
	}); err != nil {
		t.Fatalf("SaveProviders: %v", err)
	}

	recorder := performCodexNamespaceTestRequest(
		t,
		relay,
		[]byte(`{"model":"gpt-5-codex","stream":true,"input":"hi"}`),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if capacityCalls.Load() != 1 || successCalls.Load() != 1 {
		t.Fatalf("upstream calls capacity=%d success=%d", capacityCalls.Load(), successCalls.Load())
	}
	if recorder.Body.String() != successBody {
		t.Fatalf("client response changed or leaked capacity response:\n got: %q\nwant: %q", recorder.Body.String(), successBody)
	}
}

func TestCodexCapacityResponseCountsTowardFixedBlacklist(t *testing.T) {
	const capacityProvider = "capacity-fixed-primary"
	var capacityCalls atomic.Int32
	capacity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		capacityCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codexCapacitySSE("slow_down", "retry later")))
	}))
	defer capacity.Close()

	var successCalls atomic.Int32
	success := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		successCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codexSessionSSE(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"after blacklist"}]}`)))
	}))
	defer success.Close()

	providers, relay := newTestRelayService(t)
	if GlobalDBQueue == nil {
		if err := InitGlobalDBQueue(); err != nil {
			t.Fatalf("InitGlobalDBQueue: %v", err)
		}
	}
	setNamespaceRoutingDBSetting(t, "enable_blacklist", "true")
	setNamespaceRoutingDBSetting(t, "blacklist_level_enabled", "false")
	setNamespaceRoutingDBSetting(t, "blacklist_failure_threshold", "2")
	setNamespaceRoutingRetryWait(t, relay.blacklistService.settingsService, 0)
	clearNamespaceRoutingBlacklistRows(t, capacityProvider, "capacity-fixed-secondary")
	setCapacityTestDegradation(t, relay, false)
	if err := providers.SaveProviders(ProviderKindCodex, []Provider{
		{ID: 1, Name: capacityProvider, APIURL: capacity.URL, APIKey: "key-1", Enabled: true, Level: 1},
		{ID: 2, Name: "capacity-fixed-secondary", APIURL: success.URL, APIKey: "key-2", Enabled: true, Level: 2},
	}); err != nil {
		t.Fatalf("SaveProviders: %v", err)
	}

	recorder := performCodexNamespaceTestRequest(
		t,
		relay,
		[]byte(`{"model":"gpt-5-codex","stream":true,"input":"hi"}`),
	)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "after blacklist") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if capacityCalls.Load() != 2 || successCalls.Load() != 1 {
		t.Fatalf("upstream calls capacity=%d success=%d, want 2/1", capacityCalls.Load(), successCalls.Load())
	}
	if blacklisted, _ := relay.blacklistService.IsBlacklisted(ProviderKindCodex, capacityProvider); !blacklisted {
		t.Fatal("capacity provider was not blacklisted after reaching the failure threshold")
	}
}

func TestCodexDegradationBufferDetectsCapacityAfterOutput(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		body := codexCapacityAfterOutputSSE("server_is_overloaded")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	relay, _ := newDegradationTestRelay(t, 3, []int{516})
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestBody := []byte(`{"model":"gpt-5-codex","stream":true,"input":"hi"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(requestBody))

	ok, err := relay.forwardCodexWithDegradationRetry(
		c,
		Provider{Name: "capacity-after-output", APIURL: upstream.URL, APIKey: "key"},
		"/responses",
		nil,
		map[string]string{"Content-Type": "application/json"},
		requestBody,
		true,
		"gpt-5-codex",
	)
	if ok {
		t.Fatal("capacity response was treated as success")
	}
	var capacityErr *codexProviderCapacityError
	if !errors.As(err, &capacityErr) || capacityErr.Code != "server_is_overloaded" {
		t.Fatalf("error=%v capacity=%#v", err, capacityErr)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d, capacity must not consume degradation resend attempts", calls.Load())
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("capacity response leaked to client: %q", recorder.Body.String())
	}
}
