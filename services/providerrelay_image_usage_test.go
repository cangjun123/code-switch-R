package services

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamOpenAIImageResponseCapturesUsage(t *testing.T) {
	body := `data: {"type":"image_generation.partial_image","b64_json":"aaa"}` + "\n\n" +
		`data: {"type":"image_generation.completed","b64_json":"bbb","usage":{"total_tokens":300,"input_tokens":50,"output_tokens":250,"input_tokens_details":{"text_tokens":50,"image_tokens":0}}}` + "\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: ioNopCloser(body)}
	log := &ReqeustLog{}
	if err := streamOpenAIImageResponse(httptest.NewRecorder(), resp, true, log, time.Now()); err != nil {
		t.Fatal(err)
	}
	if log.InputTokens != 50 || log.OutputTokens != 250 {
		t.Fatalf("image stream usage not captured: in=%d out=%d", log.InputTokens, log.OutputTokens)
	}
}

func blacklistFailureCount(t *testing.T, relay *ProviderRelayService, kind, name string) int {
	t.Helper()
	statuses, err := relay.blacklistService.GetBlacklistStatus(kind)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range statuses {
		if status.ProviderName == name {
			return status.FailureCount
		}
	}
	return 0
}

func TestImageContentPolicy400NotBlacklisted(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Your request was rejected by the safety system.","code":"moderation_blocked"}}`))
	}))
	defer upstream.Close()
	router, relay := newImageAccessEnv(t, []Provider{
		{ID: 1, Name: "Img400A", APIURL: upstream.URL, APIKey: "a", Enabled: true, Level: 1},
		{ID: 2, Name: "Img400B", APIURL: upstream.URL, APIKey: "b", Enabled: true, Level: 2},
	})
	key, err := relay.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatal(err)
	}
	w := doImageRequest(router, key.Key)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "moderation_blocked") {
		t.Fatalf("expected upstream 400 passthrough, got %d: %s", w.Code, w.Body.String())
	}
	if hits != 2 {
		t.Fatalf("expected failover across both providers, hits=%d", hits)
	}
	if n := blacklistFailureCount(t, relay, ProviderKindGPTImage, "Img400A"); n != 0 {
		t.Fatalf("400 request error must not count toward blacklist, failure_count=%d", n)
	}
}

func TestImageHealthCheckDefaultsToModelsProbe(t *testing.T) {
	var method, path string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-image-2"}]}`))
	}))
	defer upstream.Close()
	providerService, _ := newTestRelayService(t)
	if err := providerService.SaveProviders(ProviderKindGPTImage, []Provider{
		{ID: 1, Name: "ProbeImg", APIURL: upstream.URL, APIKey: "k", Enabled: true, AvailabilityMonitorEnabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	hcs := NewHealthCheckService(providerService, nil, nil)
	result, err := hcs.RunSingleCheck(ProviderKindGPTImage, 1)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || path != "/v1/models" || result.Status != HealthStatusOperational {
		t.Fatalf("expected GET /v1/models probe, got %s %s status=%s", method, path, result.Status)
	}

	cts := NewConnectivityTestService(providerService, nil, nil)
	manual := cts.TestProviderManual(ProviderKindGPTImage, upstream.URL, "k", "", "/v1/models", "bearer")
	if !manual.Success || method != http.MethodGet {
		t.Fatalf("manual image connectivity test should GET models: %+v method=%s", manual, method)
	}

	// 生产端点即生图接口、未单独配置测试端点时走真实生图，并使用更长的默认超时
	provider := Provider{APIEndpoint: "/v1/images/generations"}
	if got := hcs.getEffectiveTimeout(&provider, ProviderKindGPTImage); got != DefaultImageProbeTimeoutMs {
		t.Fatalf("generation probe timeout = %d", got)
	}
}

func ioNopCloser(s string) *nopReadCloser { return &nopReadCloser{strings.NewReader(s)} }

type nopReadCloser struct{ *strings.Reader }

func (nopReadCloser) Close() error { return nil }

func TestLegacyDefaultImageProbeUsesModelList(t *testing.T) {
	hcs := NewHealthCheckService(NewProviderService(), nil, nil)
	legacy := Provider{AvailabilityConfig: &AvailabilityConfig{TestEndpoint: "/v1/images/generations", Timeout: 15000}}
	if got := hcs.getEffectiveEndpoint(&legacy, ProviderKindGPTImage); got != defaultImageProbeEndpoint {
		t.Fatalf("legacy default image probe should use %s, got %s", defaultImageProbeEndpoint, got)
	}
	custom := Provider{AvailabilityConfig: &AvailabilityConfig{TestEndpoint: "/v1/images/generations", Timeout: 90000}}
	if got := hcs.getEffectiveEndpoint(&custom, ProviderKindGPTImage); got != "/v1/images/generations" {
		t.Fatalf("explicit generation probe must be kept, got %s", got)
	}
	if got := hcs.getEffectiveTimeout(&custom, ProviderKindGPTImage); got != 90000 {
		t.Fatalf("explicit timeout must be kept, got %d", got)
	}
	// 非生图平台不受影响
	claude := Provider{AvailabilityConfig: &AvailabilityConfig{TestEndpoint: "/v1/images/generations", Timeout: 15000}}
	if got := hcs.getEffectiveEndpoint(&claude, ProviderKindClaude); got != "/v1/images/generations" {
		t.Fatalf("legacy migration must only apply to gpt-image, got %s", got)
	}
}

func TestDedicatedImageKeyListsImageModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()
	router, relay := newImageAccessEnv(t, []Provider{
		{ID: 1, Name: "ImgA", APIURL: upstream.URL, APIKey: "a", Enabled: true, SupportedModels: map[string]bool{"image-model-a": true}},
		{ID: 2, Name: "ImgB", APIURL: upstream.URL, APIKey: "b", Enabled: true, SupportedModels: map[string]bool{"image-model-b": true}},
	})
	key, _, err := relay.codexRelayKeys.EnsureProviderKey(ProviderKindGPTImage, 2, "GPT Image · ImgB")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key.Key)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "image-model-b") || strings.Contains(w.Body.String(), "image-model-a") {
		t.Fatalf("dedicated image key should list only its image models, got %d: %s", w.Code, w.Body.String())
	}
}
