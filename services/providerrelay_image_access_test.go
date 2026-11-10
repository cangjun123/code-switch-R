package services

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

func imageUpstream(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"aW1hZ2U="}]}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func newImageAccessEnv(t *testing.T, providers []Provider) (*gin.Engine, *ProviderRelayService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	providerService, relay := newTestRelayService(t)
	if err := providerService.SaveProviders(ProviderKindGPTImage, providers); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	relay.registerRoutes(router)
	return router, relay
}

func doImageRequest(router *gin.Engine, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"apple"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestImageRelayKeyAllowlist(t *testing.T) {
	var hitsA, hitsB int32
	upA, upB := imageUpstream(t, &hitsA), imageUpstream(t, &hitsB)
	router, relay := newImageAccessEnv(t, []Provider{
		{ID: 1, Name: "ImgA", APIURL: upA.URL, APIKey: "a", Enabled: true, Level: 1},
		{ID: 2, Name: "ImgB", APIURL: upB.URL, APIKey: "b", Enabled: true, Level: 2},
	})
	key, err := relay.codexRelayKeys.CreateKey("image-only-b")
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.codexRelayKeys.UpdateAllowedImageProviderIDs(key.ID, []int64{2}); err != nil {
		t.Fatal(err)
	}
	if w := doImageRequest(router, key.Key); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if hitsA != 0 || hitsB != 1 {
		t.Fatalf("image allowlist not applied: hitsA=%d hitsB=%d", hitsA, hitsB)
	}

	if err := relay.codexRelayKeys.UpdateAllowedImageProviderIDs(key.ID, []int64{99}); err != nil {
		t.Fatal(err)
	}
	w := doImageRequest(router, key.Key)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "provider_access_denied") {
		t.Fatalf("expected 403 provider_access_denied, got %d: %s", w.Code, w.Body.String())
	}
	if hitsA != 0 || hitsB != 1 {
		t.Fatalf("denied request reached upstream: hitsA=%d hitsB=%d", hitsA, hitsB)
	}
}

func TestImageDedicatedKey(t *testing.T) {
	var hitsA, hitsB int32
	upA, upB := imageUpstream(t, &hitsA), imageUpstream(t, &hitsB)
	router, relay := newImageAccessEnv(t, []Provider{
		{ID: 1, Name: "ImgA", APIURL: upA.URL, APIKey: "a", Enabled: true, Level: 1},
		{ID: 2, Name: "ImgB", APIURL: upB.URL, APIKey: "b", Enabled: true, Level: 2},
	})
	key, created, err := relay.codexRelayKeys.EnsureProviderKey(ProviderKindGPTImage, 2, "GPT Image · ImgB")
	if err != nil || !created {
		t.Fatalf("EnsureProviderKey(gpt-image): created=%v err=%v", created, err)
	}
	if w := doImageRequest(router, key.Key); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if hitsA != 0 || hitsB != 1 {
		t.Fatalf("dedicated image key reached wrong provider: hitsA=%d hitsB=%d", hitsA, hitsB)
	}

	// 专属生图 Key 不能访问 Codex 推理接口
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+key.Key)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("dedicated image key should be denied on /v1/responses, got %d: %s", w.Code, w.Body.String())
	}

	removed, err := relay.codexRelayKeys.PruneProviderKeys(ProviderKindGPTImage, []int64{1})
	if err != nil || len(removed) != 1 {
		t.Fatalf("prune gpt-image keys: removed=%v err=%v", removed, err)
	}
}

func TestModelsHandlerFiltersImageModelsByKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()
	router, relay := newImageAccessEnv(t, []Provider{
		{ID: 1, Name: "ImgA", APIURL: upstream.URL, APIKey: "a", Enabled: true, SupportedModels: map[string]bool{"image-model-a": true}},
		{ID: 2, Name: "ImgB", APIURL: upstream.URL, APIKey: "b", Enabled: true, SupportedModels: map[string]bool{"image-model-b": true}},
	})
	key, err := relay.codexRelayKeys.CreateKey("models-only-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.codexRelayKeys.UpdateAllowedImageProviderIDs(key.ID, []int64{1}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key.Key)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "image-model-a") || strings.Contains(w.Body.String(), "image-model-b") {
		t.Fatalf("image models not filtered by key allowlist: %s", w.Body.String())
	}
}
