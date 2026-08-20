package services

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/daodao97/xgo/xrequest"
	"github.com/gin-gonic/gin"
)

// ==================== codexStreamLacksTerminalEvent ====================

func TestCodexStreamLacksTerminalEvent(t *testing.T) {
	sawPayloads := &codexResponseObservation{jsonPayloads: 3, terminal: false}
	sawTerminal := &codexResponseObservation{jsonPayloads: 3, terminal: true}
	noPayloads := &codexResponseObservation{jsonPayloads: 0, terminal: false}

	cases := []struct {
		name        string
		observation *codexResponseObservation
		kind        string
		endpoint    string
		streamed    bool
		want        bool
	}{
		{"截断：有数据无终止事件", sawPayloads, "codex", "/responses", true, true},
		{"正常：有终止事件", sawTerminal, "codex", "/responses", true, false},
		{"空流：无数据事件 fail-open", noPayloads, "codex", "/responses", true, false},
		{"nil observation fail-open", nil, "codex", "/responses", true, false},
		{"count_tokens 排除", sawPayloads, "codex", "/responses/count_tokens", true, false},
		{"非 /responses 入口", sawPayloads, "codex", "/v1/chat/completions", true, false},
		{"claude 平台排除", sawPayloads, "claude", "/responses", true, false},
		{"非流式排除", sawPayloads, "codex", "/responses", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := codexStreamLacksTerminalEvent(c.observation, c.kind, c.endpoint, c.streamed); got != c.want {
				t.Fatalf("codexStreamLacksTerminalEvent() = %v, want %v", got, c.want)
			}
		})
	}
}

// ==================== forwardRequest 集成测试 ====================

// runForwardRequest 起一个固定返回 sseBody 的假上游，跑一次 forwardRequest（codex /responses 透传路径）。
func runForwardRequest(t *testing.T, sseBody string) (bool, error, *httptest.ResponseRecorder) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody))
	}))
	defer server.Close()

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5-codex","input":"hi","stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(body))

	prs := &ProviderRelayService{httpClient: newRelayHTTPClient()}
	provider := Provider{Name: "truncation-test", APIURL: server.URL}
	headers := map[string]string{"Content-Type": "application/json"}
	ok, err := prs.forwardRequest(c, "codex", provider, "/responses", nil, headers, body, true, "gpt-5-codex")
	return ok, err, rec
}

// truncatedCodexSSE 模拟上游中途掐断：只有输出 delta，没有 response.completed 终止事件。
func truncatedCodexSSE() string {
	return "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
}

func TestForwardRequest_TruncatedStreamReturnsSentinel(t *testing.T) {
	ok, err, rec := runForwardRequest(t, truncatedCodexSSE())
	if !ok {
		t.Fatalf("ok=false（截断时响应已发出应仍视为 ok=true）, err=%v", err)
	}
	if !errors.Is(err, errStreamTruncated) {
		t.Fatalf("err=%v, want errStreamTruncated", err)
	}
	// 客户端应已收到截断的原始流（透传行为不变）
	if !strings.Contains(rec.Body.String(), "response.output_text.delta") {
		t.Fatalf("客户端应收到已透传的截断流, got %q", rec.Body.String())
	}
}

func TestForwardRequest_CompletedStreamReturnsNil(t *testing.T) {
	ok, err, _ := runForwardRequest(t, string(codexSSE(800)))
	if !ok || err != nil {
		t.Fatalf("正常完整流应返回 (true, nil), got (%v, %v)", ok, err)
	}
}

// ==================== forwardCodexWithDegradationRetry 集成测试 ====================

// runDegradationTruncationRetry 降智检测开启（但特征值永不命中）下，最终尝试返回截断流。
func runDegradationTruncationRetry(t *testing.T, maxResend int, sseBody string) (bool, error, *httptest.ResponseRecorder) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody))
	}))
	defer server.Close()

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5-codex","input":"hi","stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(body))

	prs, _ := newDegradationTestRelay(t, maxResend, []int{516})
	provider := Provider{Name: "truncation-buffered", APIURL: server.URL}
	headers := map[string]string{"Content-Type": "application/json"}
	ok, err := prs.forwardCodexWithDegradationRetry(c, provider, "/responses", nil, headers, body, true, "gpt-5-codex")
	return ok, err, rec
}

func TestForwardCodexWithDegradationRetry_TruncatedStreamReturnsSentinel(t *testing.T) {
	ok, err, rec := runDegradationTruncationRetry(t, 1, truncatedCodexSSE())
	if !ok {
		t.Fatalf("ok=false（截断时缓冲内容已发出应仍视为 ok=true）, err=%v", err)
	}
	if !errors.Is(err, errStreamTruncated) {
		t.Fatalf("err=%v, want errStreamTruncated", err)
	}
	if !strings.Contains(rec.Body.String(), "response.output_text.delta") {
		t.Fatalf("客户端应收到已写出的截断流, got %q", rec.Body.String())
	}
}

func TestForwardCodexWithDegradationRetry_CompletedStreamReturnsNil(t *testing.T) {
	// 回归：带 response.completed 的完整流不受截断检测影响
	ok, err, _ := runDegradationTruncationRetry(t, 1, string(codexSSE(800)))
	if !ok || err != nil {
		t.Fatalf("完整流应返回 (true, nil), got (%v, %v)", ok, err)
	}
}

// ==================== observation 对真实截断流的判定 ====================

// TestObservationDetectsTruncationOverStream 验证 hook 链对截断流的实际累计结果：
// 数据事件计数 > 0 且 terminal 保持 false。
func TestObservationDetectsTruncationOverStream(t *testing.T) {
	observation := codexResponseObservation{}
	for _, line := range bytes.Split([]byte(truncatedCodexSSE()), []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		observation.Hook()(trimmed)
	}
	if observation.jsonPayloads == 0 {
		t.Fatal("截断流应累计到 jsonPayloads > 0")
	}
	if observation.terminal {
		t.Fatal("截断流不应出现终止事件")
	}
	if !codexStreamLacksTerminalEvent(&observation, "codex", "/responses", true) {
		t.Fatal("截断流应判定为 lacks terminal event")
	}
}

// ==================== 竞态保护（截断判定不引入共享状态） ====================

func TestStreamTruncatedSentinelIsStable(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			wrapped := fmt.Errorf("upstream status 200: %w", errStreamTruncated)
			if !errors.Is(wrapped, errStreamTruncated) {
				t.Errorf("wrapped sentinel #%d not detected", n)
			}
		}(i)
	}
	wg.Wait()
}

// ==================== xrequest.Response 构造工具 ====================

var _ = xrequest.NewResponse // 保持导入（供后续扩展使用同款构造方式）
