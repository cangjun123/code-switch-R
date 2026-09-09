package services

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// newGeminiRelayTestEnv 构建带一个 Gemini provider 的测试 relay 环境，返回路由和 relay key
// providerID 需要每次调用时唯一（GeminiService 的 providers 会持久化到磁盘，跨测试累积）
func newGeminiRelayTestEnv(t *testing.T, providerID, upstreamURL string) (*gin.Engine, *CodexRelayKey) {
	t.Helper()

	_, relayService := newTestRelayService(t)
	if err := relayService.geminiService.AddProvider(GeminiProvider{
		ID:      providerID,
		Name:    "GeminiProvider",
		BaseURL: upstreamURL,
		APIKey:  "provider-api-key",
		Model:   "gemini-2.5-pro",
		Enabled: true,
		Level:   1,
	}); err != nil {
		t.Fatalf("添加 Gemini provider 失败: %v", err)
	}

	relayKey, err := relayService.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatalf("创建 relay key 失败: %v", err)
	}

	router := gin.New()
	relayService.registerRoutes(router)
	return router, relayKey
}

// TestGeminiProxyRequiresManagedKey 验证 Gemini 路由强制 relay key 认证，且 relay key 不泄漏给上游
func TestGeminiProxyRequiresManagedKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamHits := 0
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++

		if r.Method != http.MethodPost {
			t.Errorf("期望 POST 请求，收到 %s", r.Method)
		}
		if r.URL.Path != "/v1beta/models/gemini-2.5-pro:generateContent" {
			t.Errorf("期望路径 /v1beta/models/gemini-2.5-pro:generateContent，收到 %s", r.URL.Path)
		}
		// 上游必须收到 provider 的 key，而非客户端的 relay key
		if got := r.Header.Get("x-goog-api-key"); got != "provider-api-key" {
			t.Errorf("上游 x-goog-api-key 应为 provider key，收到 %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("客户端 Authorization 头不应透传到上游，收到 %q", got)
		}
		// 客户端通过 ?key= 传入的 relay key 不应出现在上游查询串中
		if got := r.URL.Query().Get("key"); got != "" {
			t.Errorf("上游查询串不应包含 key=，收到 %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"totalTokenCount":5}}`))
	}))
	defer upstreamServer.Close()

	router, relayKey := newGeminiRelayTestEnv(t, "g1", upstreamServer.URL)

	body := `{"contents":[{"parts":[{"text":"hi"}]}]}`
	makeRequest := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/models/gemini-2.5-pro:generateContent?alt=sse", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("x-goog-api-key", key)
			req.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// 缺少 key → 401，且响应头带 Gemini realm
	w := makeRequest("")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("缺少 key 时应返回 401，实际为 %d，响应体: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("WWW-Authenticate"); !strings.Contains(got, "code-switch-gemini") {
		t.Errorf("401 响应应携带 WWW-Authenticate realm=code-switch-gemini，收到 %q", got)
	}

	// 错误 key → 401
	if w := makeRequest("wrong-key"); w.Code != http.StatusUnauthorized {
		t.Fatalf("错误 key 时应返回 401，实际为 %d，响应体: %s", w.Code, w.Body.String())
	}

	// 正确 key（x-goog-api-key 头）→ 200
	w = makeRequest(relayKey.Key)
	if w.Code != http.StatusOK {
		t.Fatalf("正确 key 时应返回 200，实际为 %d，响应体: %s", w.Code, w.Body.String())
	}

	if upstreamHits != 1 {
		t.Fatalf("只有合法请求才应命中上游，实际命中 %d 次", upstreamHits)
	}
}

// TestGeminiProxyQueryKeyAuth 验证通过 ?key= 查询参数认证，且 key 被剔除后不再转发上游
func TestGeminiProxyQueryKeyAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-goog-api-key"); got != "provider-api-key" {
			t.Errorf("上游 x-goog-api-key 应为 provider key，收到 %q", got)
		}
		if got := r.URL.Query().Get("key"); got != "" {
			t.Errorf("上游查询串不应包含 key=，收到 %q", got)
		}
		// 其他查询参数保留
		if got := r.URL.Query().Get("alt"); got != "sse" {
			t.Errorf("alt=sse 应保留，收到 %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`))
	}))
	defer upstreamServer.Close()

	router, relayKey := newGeminiRelayTestEnv(t, "g2", upstreamServer.URL)

	req := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/models/gemini-2.5-pro:generateContent?key="+relayKey.Key+"&alt=sse", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("query key 认证应返回 200，实际为 %d，响应体: %s", w.Code, w.Body.String())
	}
}

// TestGeminiModelsEndpointRequiresKey 验证 GET 模型列表端点的认证与转发
func TestGeminiModelsEndpointRequiresKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("期望 GET 请求，收到 %s", r.Method)
		}
		if r.URL.Path != "/v1beta/models" && r.URL.Path != "/v1/models" {
			t.Errorf("期望路径 /v1beta/models 或 /v1/models，收到 %s", r.URL.Path)
		}
		if got := r.Header.Get("x-goog-api-key"); got != "provider-api-key" {
			t.Errorf("上游 x-goog-api-key 应为 provider key，收到 %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-2.5-pro"}]}`))
	}))
	defer upstreamServer.Close()

	router, relayKey := newGeminiRelayTestEnv(t, "g3", upstreamServer.URL)

	// 无 key → 401
	req := httptest.NewRequest(http.MethodGet, "/gemini/v1beta/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("缺少 key 时应返回 401，实际为 %d，响应体: %s", w.Code, w.Body.String())
	}

	// 带 key → 200，模型列表透传
	req = httptest.NewRequest(http.MethodGet, "/gemini/v1beta/models", nil)
	req.Header.Set("x-goog-api-key", relayKey.Key)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("正确 key 时应返回 200，实际为 %d，响应体: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "models/gemini-2.5-pro") {
		t.Errorf("模型列表应透传，收到 %s", w.Body.String())
	}

	// /v1 版本同样可用
	req = httptest.NewRequest(http.MethodGet, "/gemini/v1/models", nil)
	req.Header.Set("x-goog-api-key", relayKey.Key)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/gemini/v1/models 应返回 200，实际为 %d，响应体: %s", w.Code, w.Body.String())
	}
}
