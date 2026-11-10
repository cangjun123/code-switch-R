package services

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// newGeminiParityEnv 构建只含给定 providers 的 Gemini relay 环境（先清空跨测试持久化的 providers）
func newGeminiParityEnv(t *testing.T, providers ...GeminiProvider) (*gin.Engine, *ProviderRelayService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	_, relayService := newTestRelayService(t)
	for _, existing := range relayService.geminiService.GetProviders() {
		if err := relayService.geminiService.DeleteProvider(existing.ID); err != nil {
			t.Fatalf("清理 Gemini provider 失败: %v", err)
		}
	}
	for _, provider := range providers {
		if err := relayService.geminiService.AddProvider(provider); err != nil {
			t.Fatalf("添加 Gemini provider 失败: %v", err)
		}
	}
	router := gin.New()
	relayService.registerRoutes(router)
	return router, relayService
}

func geminiTestProvider(id, name, baseURL string) GeminiProvider {
	return GeminiProvider{ID: id, Name: name, BaseURL: baseURL, APIKey: "upstream-" + id, Enabled: true, Level: 1}
}

func countingGeminiUpstream(t *testing.T, hits *int32, paths *[]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		if paths != nil {
			*paths = append(*paths, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func doGeminiRequest(router *gin.Engine, key, model string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/models/"+model+":generateContent", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", key)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestGeminiRelayKeyAllowlistFiltersProviders(t *testing.T) {
	var hitsA, hitsB int32
	upA := countingGeminiUpstream(t, &hitsA, nil)
	upB := countingGeminiUpstream(t, &hitsB, nil)
	router, relay := newGeminiParityEnv(t,
		geminiTestProvider("allow-a", "A", upA.URL),
		geminiTestProvider("allow-b", "B", upB.URL),
	)

	key, err := relay.codexRelayKeys.CreateKey("gemini-only-b")
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.codexRelayKeys.UpdateAllowedGeminiProviderIDs(key.ID, []string{"allow-b"}); err != nil {
		t.Fatal(err)
	}
	if w := doGeminiRequest(router, key.Key, "gemini-2.5-pro"); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if hitsA != 0 || hitsB != 1 {
		t.Fatalf("allowlist not applied: hitsA=%d hitsB=%d", hitsA, hitsB)
	}

	// 白名单内的 provider 全部被删除 → 403，且不命中其他 provider
	if err := relay.codexRelayKeys.UpdateAllowedGeminiProviderIDs(key.ID, []string{"missing"}); err != nil {
		t.Fatal(err)
	}
	w := doGeminiRequest(router, key.Key, "gemini-2.5-pro")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if gjson.Get(w.Body.String(), "error.status").String() != "PERMISSION_DENIED" {
		t.Fatalf("expected Google-style error body, got %s", w.Body.String())
	}
	if hitsA != 0 || hitsB != 1 {
		t.Fatalf("denied request reached upstream: hitsA=%d hitsB=%d", hitsA, hitsB)
	}
}

func TestGeminiDedicatedKeyBoundToOneProvider(t *testing.T) {
	var hitsA, hitsB int32
	upA := countingGeminiUpstream(t, &hitsA, nil)
	upB := countingGeminiUpstream(t, &hitsB, nil)
	router, relay := newGeminiParityEnv(t,
		geminiTestProvider("bound-a", "A", upA.URL),
		geminiTestProvider("bound-b", "B", upB.URL),
	)

	key, created, err := relay.codexRelayKeys.EnsureGeminiProviderKey("bound-b", "Gemini · B")
	if err != nil || !created {
		t.Fatalf("EnsureGeminiProviderKey: created=%v err=%v", created, err)
	}
	again, created, err := relay.codexRelayKeys.EnsureGeminiProviderKey("bound-b", "Gemini · B")
	if err != nil || created || again.ID != key.ID {
		t.Fatalf("second ensure should reuse key: created=%v err=%v", created, err)
	}
	if w := doGeminiRequest(router, key.Key, "gemini-2.5-pro"); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if hitsA != 0 || hitsB != 1 {
		t.Fatalf("dedicated key reached wrong provider: hitsA=%d hitsB=%d", hitsA, hitsB)
	}

	// 删除供应商后专属 Key 被清理
	removed, err := relay.codexRelayKeys.PruneGeminiProviderKeys([]string{"bound-a"})
	if err != nil || len(removed) != 1 || removed[0] != key.ID {
		t.Fatalf("prune: removed=%v err=%v", removed, err)
	}
	if found, _ := relay.codexRelayKeys.FindKey(key.Key); found != nil {
		t.Fatal("pruned key should no longer authenticate")
	}
}

func TestGeminiModelWhitelistAndMapping(t *testing.T) {
	var hitsA, hitsB int32
	var pathsB []string
	upA := countingGeminiUpstream(t, &hitsA, nil)
	upB := countingGeminiUpstream(t, &hitsB, &pathsB)
	providerA := geminiTestProvider("model-a", "A", upA.URL)
	providerA.SupportedModels = map[string]bool{"gemini-1.5-*": true}
	providerB := geminiTestProvider("model-b", "B", upB.URL)
	providerB.Level = 2
	providerB.ModelMapping = map[string]string{"gemini-2.5-*": "vendor/gemini-2.5-*"}
	router, relay := newGeminiParityEnv(t, providerA, providerB)
	key, err := relay.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatal(err)
	}

	if w := doGeminiRequest(router, key.Key, "gemini-2.5-pro"); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if hitsA != 0 {
		t.Fatalf("provider A does not whitelist gemini-2.5-pro but was hit %d times", hitsA)
	}
	if len(pathsB) != 1 || pathsB[0] != "/v1beta/models/vendor/gemini-2.5-pro:generateContent" {
		t.Fatalf("model mapping not applied to path: %v", pathsB)
	}

	// 没有任何 provider 支持的模型 → 404 Google 格式错误
	w := doGeminiRequest(router, key.Key, "imagen-3")
	if w.Code != http.StatusNotFound || gjson.Get(w.Body.String(), "error.code").Int() != 404 {
		t.Fatalf("expected 404 Google error, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGeminiClientErrorFailsOverAndPassesThrough(t *testing.T) {
	var hits int32
	badRequest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Invalid JSON payload","status":"INVALID_ARGUMENT"}}`))
	}))
	defer badRequest.Close()
	providerA := geminiTestProvider("client-err-a", "ClientErrA", badRequest.URL)
	providerB := geminiTestProvider("client-err-b", "ClientErrB", badRequest.URL)
	providerB.Level = 2
	router, relay := newGeminiParityEnv(t, providerA, providerB)
	key, err := relay.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatal(err)
	}

	w := doGeminiRequest(router, key.Key, "gemini-2.5-pro")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Invalid JSON payload") {
		t.Fatalf("expected upstream 400 passthrough, got %d: %s", w.Code, w.Body.String())
	}
	if hits != 2 {
		t.Fatalf("expected failover to second provider, upstream hits=%d", hits)
	}
}

func TestGeminiUpstreamStatusErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		body   string
		client bool
	}{
		{400, `{"error":{"message":"Invalid JSON payload"}}`, true},
		{413, `too large`, true},
		{400, `{"error":{"message":"API key not valid. Please pass a valid API key.","details":[{"reason":"API_KEY_INVALID"}]}}`, false},
		{429, `quota`, false},
		{500, `boom`, false},
	}
	for _, tc := range cases {
		err := &geminiUpstreamStatusError{StatusCode: tc.status, Body: tc.body}
		if got := err.isClientRequestError(); got != tc.client {
			t.Errorf("status %d body %q: isClientRequestError=%v, want %v", tc.status, tc.body, got, tc.client)
		}
	}
}

func TestRewriteGeminiEndpointModel(t *testing.T) {
	cases := map[string]string{
		"/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse": "/v1beta/models/mapped:streamGenerateContent?alt=sse",
		"/v1beta/models/gemini-2.5-pro:generateContent":               "/v1beta/models/mapped:generateContent",
		"/v1beta/models/gemini-2.5-pro":                               "/v1beta/models/mapped",
		"/v1beta/models":                                              "/v1beta/models",
	}
	for input, want := range cases {
		if got := rewriteGeminiEndpointModel(input, "mapped"); got != want {
			t.Errorf("rewrite(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestGeminiStreamObservationTruncation(t *testing.T) {
	observe := func(body string) *geminiStreamObservation {
		o := &geminiStreamObservation{}
		var out strings.Builder
		if err := streamGeminiResponseWithHook(strings.NewReader(body), &out, &ReqeustLog{}, time.Now(), geminiStreamOptions{sanitize: true, observation: o}); err != nil {
			t.Fatal(err)
		}
		return o
	}
	partial := `data: {"candidates":[{"content":{"parts":[{"text":"he"}]}}]}` + "\n\n"
	final := `data: {"candidates":[{"content":{"parts":[{"text":"llo"}]},"finishReason":"STOP"}]}` + "\n\n"

	if o := observe(partial + final); o.truncated() {
		t.Fatal("complete stream reported as truncated")
	}
	if o := observe(partial); !o.truncated() {
		t.Fatal("stream without finishReason should be truncated")
	}
	if o := observe(": ping\n\n"); o.truncated() {
		t.Fatal("stream without candidates must fail open")
	}
	o := observe(partial + `data: {"error":{"code":503,"message":"overloaded"}}` + "\n\n")
	if o.errorMessage != "overloaded" || o.truncated() {
		t.Fatalf("in-stream error not captured: %+v", o)
	}
}

func TestGeminiEnableProxyWritesRealRelayKey(t *testing.T) {
	_, relay := newTestRelayService(t)
	svc := relay.geminiService
	if err := svc.EnableProxy(); err != nil {
		t.Fatalf("EnableProxy: %v", err)
	}
	defer func() { _ = svc.DisableProxy() }()

	key, err := relay.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatal(err)
	}
	env, err := readGeminiEnv()
	if err != nil {
		t.Fatal(err)
	}
	if env[geminiAPIKeyKey] != key.Key {
		t.Fatalf("GEMINI_API_KEY = %q, want relay key", env[geminiAPIKeyKey])
	}

	// 旧版本写入的占位 Key 在读取代理状态时被自动替换
	env[geminiAPIKeyKey] = geminiProxyTokenVal
	if err := writeGeminiEnv(env); err != nil {
		t.Fatal(err)
	}
	if status, err := svc.ProxyStatus(); err != nil || !status.Enabled {
		t.Fatalf("ProxyStatus: %+v %v", status, err)
	}
	env, _ = readGeminiEnv()
	if env[geminiAPIKeyKey] != key.Key {
		t.Fatalf("placeholder not migrated: %q", env[geminiAPIKeyKey])
	}

	if err := svc.DisableProxy(); err != nil {
		t.Fatal(err)
	}
	env, _ = readGeminiEnv()
	if v, ok := env[geminiAPIKeyKey]; ok && v == key.Key {
		t.Fatal("DisableProxy should remove the injected relay key")
	}
}

func TestGeminiNeverBlacklist(t *testing.T) {
	_, relay := newTestRelayService(t)
	for _, existing := range relay.geminiService.GetProviders() {
		_ = relay.geminiService.DeleteProvider(existing.ID)
	}
	provider := geminiTestProvider("never-bl", "NeverBL", "http://127.0.0.1:1")
	provider.NeverBlacklist = true
	if err := relay.geminiService.AddProvider(provider); err != nil {
		t.Fatal(err)
	}
	relay.blacklistService.SetGeminiService(relay.geminiService)
	if !relay.blacklistService.isNeverBlacklist(ProviderKindGemini, "NeverBL") {
		t.Fatal("Gemini never-blacklist flag not honored")
	}
	if relay.blacklistService.isNeverBlacklist(ProviderKindGemini, "Other") {
		t.Fatal("unknown Gemini provider must not be never-blacklist")
	}
}

// TestGeminiSSEKeepaliveThroughRelay 复现 Antigravity CLI 报错：
// 上游 200 + SSE 心跳注释 ":"，relay 转发后客户端必须只看到 data: 行。
func TestGeminiSSEKeepaliveThroughRelay(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("alt") != "sse" {
			t.Errorf("alt=sse not forwarded: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(":\n\n" +
			`data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}` + "\n\n" +
			": keepalive\n\n" +
			`data: {"candidates":[{"content":{"parts":[{"text":"!"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}` + "\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer upstream.Close()
	router, relay := newGeminiParityEnv(t, geminiTestProvider("sse-keepalive", "SSE", upstream.URL))
	key, err := relay.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse", strings.NewReader(`{"contents":[]}`))
	req.Header.Set("x-goog-api-key", key.Key)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	assertGenaiParsable(t, w.Body.String())
	if strings.Count(w.Body.String(), "data: ") != 2 {
		t.Fatalf("expected exactly the two data events, got %q", w.Body.String())
	}
}
