package services

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestJoinURLV1Normalization 验证 joinURL 对 v1 段的规整：
// 最终路径恰好包含一个 v1，不会缺失也不会重复。
func TestJoinURLV1Normalization(t *testing.T) {
	tests := []struct {
		name     string
		base     string
		endpoint string
		want     string
	}{
		// base 带 v1 + 端点带 v1：只保留一个
		{"base v1 + endpoint v1/messages", "https://host/v1", "/v1/messages", "https://host/v1/messages"},
		{"base v1 + endpoint v1/chat/completions", "https://host/v1/", "/v1/chat/completions", "https://host/v1/chat/completions"},
		{"base v1 + endpoint v1/responses", "https://host/v1", "/v1/responses", "https://host/v1/responses"},
		{"base v1 + endpoint v1/images/generations", "https://host/v1", "/v1/images/generations", "https://host/v1/images/generations"},
		{"base v1 + endpoint v1/messages/count_tokens", "https://host/v1", "/v1/messages/count_tokens", "https://host/v1/messages/count_tokens"},

		// base 带 v1 + 端点不带 v1：base 的被剥掉，端点补上，还是一个
		{"base v1 + endpoint /messages", "https://host/v1", "/messages", "https://host/v1/messages"},
		{"base v1 + endpoint /responses", "https://host/v1", "/responses", "https://host/v1/responses"},
		{"base v1 + endpoint /chat/completions", "https://host/v1", "/chat/completions", "https://host/v1/chat/completions"},

		// base 不带 v1 + 端点不带 v1：端点补上
		{"bare base + endpoint /responses", "https://host", "/responses", "https://host/v1/responses"},
		{"bare base + endpoint /chat/completions", "https://host", "/chat/completions", "https://host/v1/chat/completions"},
		{"bare base + endpoint /messages", "https://host", "/messages", "https://host/v1/messages"},
		{"bare base + endpoint /models", "https://host", "/models", "https://host/v1/models"},

		// base 不带 v1 + 端点带 v1：原样（已经规整）
		{"bare base + endpoint v1/messages", "https://host", "/v1/messages", "https://host/v1/messages"},

		// 双 v1 的 base 全部剥掉
		{"base double v1", "https://host/v1/v1", "/v1/messages", "https://host/v1/messages"},
		{"base double v1 + no-v1 endpoint", "https://host/v1/V1", "/responses", "https://host/v1/responses"},

		// 大小写不敏感
		{"base V1 uppercase", "https://host/V1", "/v1/messages", "https://host/v1/messages"},
		{"endpoint V1 uppercase", "https://host/v1", "/V1/messages", "https://host/V1/messages"},

		// 带路径的 base：只剥最后一段 v1，前缀路径保留
		{"base path + v1", "https://host/openai/v1", "/responses", "https://host/openai/v1/responses"},
		{"base path v1 v1", "https://host/openai/v1/v1", "/v1/chat/completions", "https://host/openai/v1/chat/completions"},

		// 其他版本段（v1beta/v2）端点：不补 v1，原样保留
		{"endpoint v1beta kept", "https://host", "/v1beta/models", "https://host/v1beta/models"},
		{"endpoint v2 kept", "https://host/v1", "/v2/foo/bar", "https://host/v2/foo/bar"},

		// 自定义路径（Azure 风格等）原样保留，不补 v1
		{"azure-style path untouched", "https://host/openai/deployments", "/openai/deployments/gpt-4/chat/completions", "https://host/openai/deployments/openai/deployments/gpt-4/chat/completions"},

		// 尾斜杠清理
		{"trailing slash base", "https://host/", "/v1/messages", "https://host/v1/messages"},
		{"endpoint leading slash missing", "https://host/v1", "v1/messages", "https://host/v1/messages"},

		// tasks（异步生图轮询）
		{"tasks endpoint", "https://host", "/v1/tasks/task-123", "https://host/v1/tasks/task-123"},
		{"tasks endpoint no v1", "https://host/v1", "/tasks/task-123", "https://host/v1/tasks/task-123"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			got := joinURL(tt.base, tt.endpoint)
			if got != tt.want {
				t.Fatalf("joinURL(%q, %q) = %q, want %q", tt.base, tt.endpoint, got, tt.want)
			}
		})
	}
}

// TestRelayAliasRoutesAcceptV1Variants 验证入口别名路由：
// 用户客户端 base_url 无论配不配 /v1，请求都能命中（/v1/responses、/chat/completions），
// 且上游收到的路径经过规整始终带一个 v1。
func TestRelayAliasRoutesAcceptV1Variants(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var upstreamPath atomic.Value
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		upstreamPath.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_alias","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	providerService, relay := newTestRelayService(t)
	if err := providerService.SaveProviders(ProviderKindCodex, []Provider{
		{ID: 1, Name: "alias-provider", APIURL: upstream.URL, APIKey: "key", Enabled: true, Level: 1},
	}); err != nil {
		t.Fatalf("SaveProviders() failed: %v", err)
	}
	key, err := relay.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatalf("EnsureDefaultKey() failed: %v", err)
	}

	router := gin.New()
	relay.registerRoutes(router)

	tests := []struct {
		name         string
		routePath    string
		wantUpstream string
	}{
		{"canonical /responses", "/responses", "/v1/responses"},
		{"alias /v1/responses", "/v1/responses", "/v1/responses"},
		{"canonical /v1/chat/completions", "/v1/chat/completions", "/v1/chat/completions"},
		{"alias /chat/completions", "/chat/completions", "/v1/chat/completions"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			before := upstreamHits.Load()
			req := httptest.NewRequest(http.MethodPost, tt.routePath, strings.NewReader(`{"model":"access-model","input":"hello"}`))
			req.Header.Set("Authorization", "Bearer "+key.Key)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if upstreamHits.Load() != before+1 {
				t.Fatalf("上游应被命中一次")
			}
			if got := upstreamPath.Load().(string); got != tt.wantUpstream {
				t.Fatalf("上游收到路径 %q, want %q", got, tt.wantUpstream)
			}
		})
	}
}
