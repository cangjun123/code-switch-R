package services

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tidwall/gjson"
)

// newGeminiAvailabilityEnv 返回已注入 Gemini 来源的 ProviderService 和清空后的 GeminiService
func newGeminiAvailabilityEnv(t *testing.T, providers ...GeminiProvider) (*ProviderService, *GeminiService) {
	t.Helper()
	providerService, relay := newTestRelayService(t)
	gemini := relay.geminiService
	for _, existing := range gemini.GetProviders() {
		if err := gemini.DeleteProvider(existing.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, provider := range providers {
		if err := gemini.AddProvider(provider); err != nil {
			t.Fatal(err)
		}
	}
	providerService.SetGeminiSource(gemini)
	return providerService, gemini
}

func TestGeminiNumericIDAssignedAndStable(t *testing.T) {
	_, gemini := newGeminiAvailabilityEnv(t,
		GeminiProvider{ID: "num-a", Name: "A", NumericID: 99}, // 客户端传入的值会被忽略
		GeminiProvider{ID: "num-b", Name: "B"},
	)
	providers := gemini.GetProviders()
	if providers[0].NumericID != 1 || providers[1].NumericID != 2 {
		t.Fatalf("unexpected numeric IDs: %d %d", providers[0].NumericID, providers[1].NumericID)
	}

	// 更新时客户端无法修改 NumericID
	updated := providers[1]
	updated.NumericID = 7
	if err := gemini.UpdateProvider(updated); err != nil {
		t.Fatal(err)
	}
	// 删除后新增不复用已删除的 ID（max+1）
	if err := gemini.DeleteProvider("num-a"); err != nil {
		t.Fatal(err)
	}
	if err := gemini.AddProvider(GeminiProvider{ID: "num-c", Name: "C"}); err != nil {
		t.Fatal(err)
	}
	dup, err := gemini.DuplicateProvider("num-b")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, p := range gemini.GetProviders() {
		ids[p.ID] = p.NumericID
	}
	if ids["num-b"] != 2 || ids["num-c"] != 3 || ids[dup.ID] != 4 || dup.NumericID != 4 {
		t.Fatalf("numeric IDs not stable: %v (dup %d)", ids, dup.NumericID)
	}
}

func TestProviderServiceGeminiFacade(t *testing.T) {
	providerService, gemini := newGeminiAvailabilityEnv(t, GeminiProvider{
		ID: "facade", Name: "Facade", BaseURL: "https://example.test", APIKey: "k", Enabled: true, Level: 2,
	})
	providers, err := providerService.LoadProviders(ProviderKindGemini)
	if err != nil || len(providers) != 1 {
		t.Fatalf("LoadProviders(gemini): %v %v", providers, err)
	}
	p := providers[0]
	if p.ID == 0 || p.APIURL != "https://example.test" || p.ConnectivityAuthType != "x-goog-api-key" || p.Level != 2 {
		t.Fatalf("unexpected adapted provider: %+v", p)
	}

	p.AvailabilityMonitorEnabled = true
	p.ConnectivityAutoBlacklist = true
	p.AvailabilityConfig = &AvailabilityConfig{TestModel: "gemini-2.5-pro", Timeout: 20000}
	// 不可通过通用视图修改的字段（只写回可用性字段，避免旧快照覆盖其他配置）
	p.NeverBlacklist = true
	p.Enabled = false
	p.Level = 9
	p.APIURL = "https://ignored.test"
	if err := providerService.SaveProviders(ProviderKindGemini, []Provider{p}); err != nil {
		t.Fatal(err)
	}
	stored := gemini.GetProviders()[0]
	if !stored.AvailabilityMonitorEnabled || !stored.ConnectivityAutoBlacklist ||
		stored.AvailabilityConfig == nil || stored.AvailabilityConfig.TestModel != "gemini-2.5-pro" {
		t.Fatalf("availability fields not written back: %+v", stored)
	}
	if stored.BaseURL != "https://example.test" || stored.NeverBlacklist || !stored.Enabled || stored.Level != 2 {
		t.Fatalf("non-availability fields must not change via facade: %+v", stored)
	}

	// 永不拉黑通过 GeminiService 设置后，经 facade 可见
	stored.NeverBlacklist = true
	if err := gemini.UpdateProvider(stored); err != nil {
		t.Fatal(err)
	}
	if !providerService.IsNeverBlacklist(ProviderKindGemini, "Facade") {
		t.Fatal("IsNeverBlacklist should see Gemini provider through facade")
	}
}

func TestGeminiConnectivityModelListProbeIgnoresBodyShape(t *testing.T) {
	var captured capturedProbe
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-2.5-flash"}]}`))
	}))
	defer server.Close()
	cts := NewConnectivityTestService(NewProviderService(), nil, nil)
	manual := cts.TestProviderManual(ProviderKindGemini, server.URL, "k", "", "/v1beta/models", "x-goog-api-key")
	if !manual.Success || captured.path != "/v1beta/models" {
		t.Fatalf("Gemini model list probe should succeed: %+v path=%q", manual, captured.path)
	}
}

type capturedProbe struct {
	path, authKey, authHeader string
	body                      []byte
}

func geminiProbeServer(t *testing.T, captured *capturedProbe, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.RequestURI()
		captured.authKey = r.Header.Get("x-goog-api-key")
		captured.authHeader = r.Header.Get("Authorization")
		captured.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestGeminiHealthCheckProbe(t *testing.T) {
	var captured capturedProbe
	server := geminiProbeServer(t, &captured, http.StatusOK)
	providerService, gemini := newGeminiAvailabilityEnv(t, GeminiProvider{
		ID: "hc", Name: "HC", BaseURL: server.URL, APIKey: "upstream-key", Enabled: true,
		ModelMapping:               map[string]string{"gemini-2.5-flash": "vendor-flash"},
		AvailabilityMonitorEnabled: true,
	})
	hcs := NewHealthCheckService(providerService, nil, nil)
	numericID := gemini.GetProviders()[0].NumericID

	result, err := hcs.RunSingleCheck(ProviderKindGemini, numericID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != HealthStatusOperational {
		t.Fatalf("expected operational, got %+v", result)
	}
	if captured.path != "/v1beta/models/vendor-flash:generateContent" {
		t.Fatalf("unexpected probe path %q", captured.path)
	}
	if captured.authKey != "upstream-key" || captured.authHeader != "" {
		t.Fatalf("expected x-goog-api-key auth, got key=%q auth=%q", captured.authKey, captured.authHeader)
	}
	if gjson.GetBytes(captured.body, "contents.0.parts.0.text").String() == "" {
		t.Fatalf("probe body is not a generateContent request: %s", captured.body)
	}

	// 流式探测 + 自定义模型
	providers, _ := providerService.LoadProviders(ProviderKindGemini)
	providers[0].AvailabilityConfig = &AvailabilityConfig{TestModel: "gemini-2.5-pro", Stream: true}
	if err := providerService.SaveProviders(ProviderKindGemini, providers); err != nil {
		t.Fatal(err)
	}
	if _, err := hcs.RunSingleCheck(ProviderKindGemini, numericID); err != nil {
		t.Fatal(err)
	}
	if captured.path != "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse" {
		t.Fatalf("unexpected stream probe path %q", captured.path)
	}
}

func TestGeminiHealthCheckIncludedInLatestResults(t *testing.T) {
	providerService, gemini := newGeminiAvailabilityEnv(t, GeminiProvider{ID: "timeline", Name: "Timeline", BaseURL: "http://127.0.0.1:1"})
	hcs := NewHealthCheckService(providerService, nil, nil)
	results, err := hcs.GetLatestResults()
	if err != nil {
		t.Fatal(err)
	}
	timelines := results[ProviderKindGemini]
	if len(timelines) != 1 || timelines[0].ProviderID != gemini.GetProviders()[0].NumericID {
		t.Fatalf("gemini timelines missing: %+v", timelines)
	}
}

func TestGeminiConnectivityProbe(t *testing.T) {
	var captured capturedProbe
	server := geminiProbeServer(t, &captured, http.StatusOK)
	providerService, gemini := newGeminiAvailabilityEnv(t, GeminiProvider{ID: "ct", Name: "CT", BaseURL: server.URL, APIKey: "k2", Enabled: true})
	cts := NewConnectivityTestService(providerService, nil, nil)

	result, err := cts.RunSingleTest(ProviderKindGemini, gemini.GetProviders()[0].NumericID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusAvailable {
		t.Fatalf("expected available, got %+v", result)
	}
	if captured.path != "/v1beta/models/gemini-2.5-flash:generateContent" || captured.authKey != "k2" {
		t.Fatalf("unexpected probe: path=%q key=%q", captured.path, captured.authKey)
	}

	// 编辑弹窗的手动测试：前端传入 Gemini 默认端点模板和认证头
	manual := cts.TestProviderManual(ProviderKindGemini, server.URL, "k3", "", "/v1beta/models/{model}:generateContent", "x-goog-api-key")
	if !manual.Success || captured.authKey != "k3" || captured.path != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("manual test failed: %+v path=%q", manual, captured.path)
	}
}

func TestBuildGeminiProbeRequest(t *testing.T) {
	var body map[string]any
	if err := json.Unmarshal(buildGeminiProbeRequest("hi", 1), &body); err != nil {
		t.Fatal(err)
	}
	if gjson.Get(mustJSON(t, body), "generationConfig.maxOutputTokens").Int() != 1 {
		t.Fatalf("maxOutputTokens missing: %v", body)
	}
	if got := resolveGeminiProbeEndpoint("", "m", false); got != "/v1beta/models/m:generateContent" {
		t.Fatalf("default endpoint = %q", got)
	}
	if got := resolveGeminiProbeEndpoint("/custom/path", "m", true); got != "/custom/path" {
		t.Fatalf("custom endpoint should be kept, got %q", got)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
