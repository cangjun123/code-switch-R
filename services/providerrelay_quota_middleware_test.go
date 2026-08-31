package services

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daodao97/xgo/xdb"
	"github.com/gin-gonic/gin"
)

func TestCodexQuotaMiddlewareReturns429WithPeriodicResetHeaders(t *testing.T) {
	setupRelayTestEnv(t)
	keyService := &CodexRelayKeyService{path: t.TempDir() + "/keys.json"}
	created, err := keyService.CreateKey("daily-quota")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if err := keyService.UpdateQuotaConfig(created.ID, 1, "0", RelayQuotaPeriodDaily); err != nil {
		t.Fatalf("configure key: %v", err)
	}
	key, err := keyService.GetKeyByID(created.ID)
	if err != nil {
		t.Fatalf("reload key: %v", err)
	}
	quota := NewRelayQuotaService()
	if _, err := quota.Settle(key, "middleware-daily-attempt", "fake", "middleware-unpriced-daily", RelayQuotaUsage{InputTokens: 1}); err != nil {
		t.Fatalf("settle usage: %v", err)
	}
	defer cleanupQuotaMiddlewareFixture(t, key.ID, "middleware-unpriced-daily")

	relay := &ProviderRelayService{codexRelayKeys: keyService, relayQuota: quota}
	recorder := runQuotaMiddlewareRequest(t, relay, key.ID)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429; body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.TrimSpace(recorder.Header().Get("Retry-After")) == "" || strings.TrimSpace(recorder.Header().Get("X-Quota-Reset")) == "" {
		t.Fatalf("periodic quota response missing reset headers: %v", recorder.Header())
	}
	var payload struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	if payload.Error.Type != "insufficient_quota" || payload.Error.Code != "relay_key_token_quota_exceeded" {
		t.Fatalf("unexpected quota error payload: %+v", payload)
	}
}

func TestCodexQuotaMiddlewareOnceHasNoRetryAfter(t *testing.T) {
	setupRelayTestEnv(t)
	keyService := &CodexRelayKeyService{path: t.TempDir() + "/keys.json"}
	created, err := keyService.CreateKey("once-quota")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if err := keyService.UpdateQuotaConfig(created.ID, 1, "0", RelayQuotaPeriodOnce); err != nil {
		t.Fatalf("configure key: %v", err)
	}
	key, err := keyService.GetKeyByID(created.ID)
	if err != nil {
		t.Fatalf("reload key: %v", err)
	}
	quota := NewRelayQuotaService()
	if _, err := quota.Settle(key, "middleware-once-attempt", "fake", "middleware-unpriced-once", RelayQuotaUsage{InputTokens: 1}); err != nil {
		t.Fatalf("settle usage: %v", err)
	}
	defer cleanupQuotaMiddlewareFixture(t, key.ID, "middleware-unpriced-once")

	relay := &ProviderRelayService{codexRelayKeys: keyService, relayQuota: quota}
	recorder := runQuotaMiddlewareRequest(t, relay, key.ID)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429; body=%s", recorder.Code, recorder.Body.String())
	}
	if value := recorder.Header().Get("Retry-After"); value != "" {
		t.Fatalf("once quota should not send Retry-After, got %q", value)
	}
	if value := recorder.Header().Get("X-Quota-Reset"); value != "" {
		t.Fatalf("once quota should not send X-Quota-Reset, got %q", value)
	}
	if !strings.Contains(recorder.Body.String(), "administrator reset") {
		t.Fatalf("once quota response should explain administrator reset: %s", recorder.Body.String())
	}
}

func runQuotaMiddlewareRequest(t *testing.T, relay *ProviderRelayService, keyID string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/test", func(c *gin.Context) {
		c.Set(relayKeyIDContextKey, keyID)
		c.Next()
	}, relay.codexQuotaMiddleware(), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(`{}`))
	router.ServeHTTP(recorder, request)
	return recorder
}

func cleanupQuotaMiddlewareFixture(t *testing.T, keyID, model string) {
	t.Helper()
	db, err := xdb.DB("default")
	if err != nil {
		return
	}
	_, _ = db.Exec("DELETE FROM relay_key_quota_state WHERE relay_key_id = ?", keyID)
	_, _ = db.Exec("DELETE FROM relay_key_quota_settlement WHERE relay_key_id = ?", keyID)
	_, _ = db.Exec("DELETE FROM relay_unpriced_model_seen WHERE model = ?", model)
}

func TestCodexQuotaRouteSettlesRealResponseAndBlocksNextRequest(t *testing.T) {
	providerService, relay := newTestRelayService(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","model":"route-quota-model","output":[],"usage":{"input_tokens":2,"output_tokens":3}}`))
	}))
	defer upstream.Close()

	provider := Provider{
		ID: 99101, Name: "route-quota-provider", APIURL: upstream.URL, APIKey: "upstream-key",
		Enabled: true, Level: 1, SupportedModels: map[string]bool{"route-quota-model": true},
	}
	if err := providerService.SaveProviders(ProviderKindCodex, []Provider{provider}); err != nil {
		t.Fatalf("save provider: %v", err)
	}
	key, err := relay.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatalf("ensure relay key: %v", err)
	}
	if err := relay.codexRelayKeys.UpdateQuotaConfig(key.ID, 5, "0", RelayQuotaPeriodOnce); err != nil {
		t.Fatalf("configure quota: %v", err)
	}
	key, err = relay.codexRelayKeys.GetKeyByID(key.ID)
	if err != nil {
		t.Fatalf("reload relay key: %v", err)
	}
	defer cleanupQuotaMiddlewareFixture(t, key.ID, "route-quota-model")

	router := gin.New()
	relay.registerRoutes(router)
	requestBody := strings.NewReader(`{"model":"route-quota-model","input":"hello"}`)
	request := httptest.NewRequest(http.MethodPost, "/responses", requestBody)
	request.Header.Set("Authorization", "Bearer "+key.Key)
	first := httptest.NewRecorder()
	router.ServeHTTP(first, request)
	if first.Code != http.StatusOK {
		t.Fatalf("first request status=%d body=%s", first.Code, first.Body.String())
	}
	status, err := relay.relayQuota.Status(key)
	if err != nil {
		t.Fatalf("quota status after request: %v", err)
	}
	if status.TokenUsed != 5 {
		t.Fatalf("real response usage was not settled: %+v", status)
	}

	secondRequest := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"route-quota-model"}`))
	secondRequest.Header.Set("Authorization", "Bearer "+key.Key)
	second := httptest.NewRecorder()
	router.ServeHTTP(second, secondRequest)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status=%d body=%s", second.Code, second.Body.String())
	}
}
