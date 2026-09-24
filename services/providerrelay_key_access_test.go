package services

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCodexRelayKeyProviderAllowlistFiltersBeforeRouting(t *testing.T) {
	providerService, relay := newTestRelayService(t)
	setNamespaceRoutingDBSetting(t, "enable_blacklist", "false")

	var deniedCalls atomic.Int64
	deniedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		deniedCalls.Add(1)
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer deniedUpstream.Close()

	var allowedCalls atomic.Int64
	allowedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		allowedCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-access","model":"access-model","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer allowedUpstream.Close()

	if err := providerService.SaveProviders(ProviderKindCodex, []Provider{
		{ID: 101, Name: "denied-provider", APIURL: deniedUpstream.URL, APIKey: "denied-key", Enabled: true, Level: 1},
		{ID: 202, Name: "allowed-provider", APIURL: allowedUpstream.URL, APIKey: "allowed-key", Enabled: true, Level: 2},
	}); err != nil {
		t.Fatalf("SaveProviders() failed: %v", err)
	}
	key, err := relay.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatalf("EnsureDefaultKey() failed: %v", err)
	}
	if err := relay.codexRelayKeys.UpdateAllowedProviderIDs(key.ID, []int64{202}); err != nil {
		t.Fatalf("UpdateAllowedProviderIDs() failed: %v", err)
	}
	key, err = relay.codexRelayKeys.GetKeyByID(key.ID)
	if err != nil {
		t.Fatalf("GetKeyByID() failed: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)
	request := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"access-model","input":"hello"}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if deniedCalls.Load() != 0 || allowedCalls.Load() != 1 {
		t.Fatalf("provider calls: denied=%d allowed=%d", deniedCalls.Load(), allowedCalls.Load())
	}
}

func TestCodexRelayKeyProviderAllowlistWithNoConfiguredMatchReturns403(t *testing.T) {
	providerService, relay := newTestRelayService(t)
	if err := providerService.SaveProviders(ProviderKindCodex, []Provider{
		{ID: 1, Name: "other-provider", APIURL: "https://example.invalid", APIKey: "key", Enabled: true},
	}); err != nil {
		t.Fatalf("SaveProviders() failed: %v", err)
	}
	key, err := relay.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatalf("EnsureDefaultKey() failed: %v", err)
	}
	if err := relay.codexRelayKeys.UpdateAllowedProviderIDs(key.ID, []int64{999}); err != nil {
		t.Fatalf("UpdateAllowedProviderIDs() failed: %v", err)
	}
	key, err = relay.codexRelayKeys.GetKeyByID(key.ID)
	if err != nil {
		t.Fatalf("GetKeyByID() failed: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)
	request := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"access-model"}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "provider_access_denied") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

const claudeAccessTestResponse = `{"id":"msg-access","type":"message","role":"assistant","model":"access-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
const claudeAccessTestRequest = `{"model":"access-model","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`

func TestClaudeRelayKeyProviderAllowlistFiltersBeforeRouting(t *testing.T) {
	providerService, relay := newTestRelayService(t)
	setNamespaceRoutingDBSetting(t, "enable_blacklist", "false")

	var deniedCalls atomic.Int64
	deniedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		deniedCalls.Add(1)
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer deniedUpstream.Close()

	var allowedCalls atomic.Int64
	allowedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		allowedCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(claudeAccessTestResponse))
	}))
	defer allowedUpstream.Close()

	if err := providerService.SaveProviders(ProviderKindClaude, []Provider{
		{ID: 101, Name: "denied-claude", APIURL: deniedUpstream.URL, APIKey: "denied-key", Enabled: true, Level: 1},
		{ID: 202, Name: "allowed-claude", APIURL: allowedUpstream.URL, APIKey: "allowed-key", Enabled: true, Level: 2},
	}); err != nil {
		t.Fatalf("SaveProviders() failed: %v", err)
	}
	key, err := relay.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatalf("EnsureDefaultKey() failed: %v", err)
	}
	// The Codex allowlist must not leak into Claude routing even when the
	// numeric IDs collide across kinds.
	if err := relay.codexRelayKeys.UpdateAllowedProviderIDs(key.ID, []int64{101}); err != nil {
		t.Fatalf("UpdateAllowedProviderIDs() failed: %v", err)
	}
	if err := relay.codexRelayKeys.UpdateAllowedClaudeProviderIDs(key.ID, []int64{202}); err != nil {
		t.Fatalf("UpdateAllowedClaudeProviderIDs() failed: %v", err)
	}
	key, err = relay.codexRelayKeys.GetKeyByID(key.ID)
	if err != nil {
		t.Fatalf("GetKeyByID() failed: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(claudeAccessTestRequest))
	request.Header.Set("x-api-key", key.Key)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if deniedCalls.Load() != 0 || allowedCalls.Load() != 1 {
		t.Fatalf("provider calls: denied=%d allowed=%d", deniedCalls.Load(), allowedCalls.Load())
	}
}

func TestClaudeRelayKeyUnaffectedByCodexAllowlist(t *testing.T) {
	providerService, relay := newTestRelayService(t)
	setNamespaceRoutingDBSetting(t, "enable_blacklist", "false")

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(claudeAccessTestResponse))
	}))
	defer upstream.Close()

	if err := providerService.SaveProviders(ProviderKindClaude, []Provider{
		{ID: 5, Name: "claude-open", APIURL: upstream.URL, APIKey: "key", Enabled: true},
	}); err != nil {
		t.Fatalf("SaveProviders() failed: %v", err)
	}
	key, err := relay.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatalf("EnsureDefaultKey() failed: %v", err)
	}
	if err := relay.codexRelayKeys.UpdateAllowedProviderIDs(key.ID, []int64{999}); err != nil {
		t.Fatalf("UpdateAllowedProviderIDs() failed: %v", err)
	}
	key, err = relay.codexRelayKeys.GetKeyByID(key.ID)
	if err != nil {
		t.Fatalf("GetKeyByID() failed: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(claudeAccessTestRequest))
	request.Header.Set("x-api-key", key.Key)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, calls.Load(), recorder.Body.String())
	}
}

func TestClaudeRelayKeyProviderAllowlistWithNoConfiguredMatchReturns403(t *testing.T) {
	providerService, relay := newTestRelayService(t)
	if err := providerService.SaveProviders(ProviderKindClaude, []Provider{
		{ID: 1, Name: "other-claude", APIURL: "https://example.invalid", APIKey: "key", Enabled: true},
	}); err != nil {
		t.Fatalf("SaveProviders() failed: %v", err)
	}
	key, err := relay.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatalf("EnsureDefaultKey() failed: %v", err)
	}
	if err := relay.codexRelayKeys.UpdateAllowedClaudeProviderIDs(key.ID, []int64{999}); err != nil {
		t.Fatalf("UpdateAllowedClaudeProviderIDs() failed: %v", err)
	}
	key, err = relay.codexRelayKeys.GetKeyByID(key.ID)
	if err != nil {
		t.Fatalf("GetKeyByID() failed: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(claudeAccessTestRequest))
		request.Header.Set("x-api-key", key.Key)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "permission_error") {
			t.Fatalf("%s status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestCodexRelayKeyCanQueryExhaustedQuota(t *testing.T) {
	_, relay := newTestRelayService(t)
	key, err := relay.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatalf("EnsureDefaultKey() failed: %v", err)
	}
	if err := relay.codexRelayKeys.UpdateQuotaConfig(key.ID, 5, "0", RelayQuotaPeriodOnce); err != nil {
		t.Fatalf("UpdateQuotaConfig() failed: %v", err)
	}
	key, err = relay.codexRelayKeys.GetKeyByID(key.ID)
	if err != nil {
		t.Fatalf("GetKeyByID() failed: %v", err)
	}
	if _, err := relay.relayQuota.Settle(key, key.ID+"-self-quota", "provider", "self-quota-unpriced-model", RelayQuotaUsage{InputTokens: 5}); err != nil {
		t.Fatalf("Settle() failed: %v", err)
	}
	defer cleanupQuotaMiddlewareFixture(t, key.ID, "self-quota-unpriced-model")

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)
	request := httptest.NewRequest(http.MethodGet, "/v1/quota", nil)
	request.Header.Set("Authorization", "Bearer "+key.Key)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control=%q, want no-store", recorder.Header().Get("Cache-Control"))
	}
	var status RelayQuotaStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode quota status: %v", err)
	}
	if !status.Blocked || status.TokenRemaining == nil || *status.TokenRemaining != 0 || status.TokenUsed != 5 {
		t.Fatalf("unexpected exhausted quota status: %+v", status)
	}

	invalidRequest := httptest.NewRequest(http.MethodGet, "/v1/quota", nil)
	invalidRequest.Header.Set("Authorization", "Bearer csk_invalid")
	invalid := httptest.NewRecorder()
	router.ServeHTTP(invalid, invalidRequest)
	if invalid.Code != http.StatusUnauthorized {
		t.Fatalf("invalid key status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}
