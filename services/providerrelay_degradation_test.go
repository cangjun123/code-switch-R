package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daodao97/xgo/xrequest"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// codexSSE 构造一段最简的 codex /responses 流式响应，reasoning_tokens 可控。
func codexSSE(reasoningTokens int) []byte {
	return []byte(fmt.Sprintf(
		"event: response.created\ndata: {\"type\":\"response.created\"}\n\n"+
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"output_tokens_details\":{\"reasoning_tokens\":%d}}}}\n\n",
		reasoningTokens))
}

func codexNamespaceSSE(reasoningTokens int) []byte {
	return append([]byte(
		"event: response.output_item.added\n"+
			"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"namespace\":\"agents\",\"name\":\"spawn_agent\"}}\n\n",
	), codexSSE(reasoningTokens)...)
}

func newCodexStreamResponse(body []byte) *xrequest.Response {
	return xrequest.NewResponse(&http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	})
}

type closeTrackingResponseBody struct {
	io.Reader
	closed bool
}

func (body *closeTrackingResponseBody) Close() error {
	body.closed = true
	return nil
}

func TestDetectAndNormalizeJSONResponsePreservesBodyAndClose(t *testing.T) {
	for _, test := range []struct {
		name     string
		body     []byte
		wantJSON bool
	}{
		{name: "JSON", body: []byte(" \r\n{\"output\":[]}"), wantJSON: true},
		{name: "SSE", body: []byte("event: response.created\n\ndata: {}\n\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := &closeTrackingResponseBody{Reader: bytes.NewReader(test.body)}
			resp := xrequest.NewResponse(&http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       original,
			})

			if got := detectAndNormalizeJSONResponse(resp, true); got != test.wantJSON {
				t.Fatalf("detectAndNormalizeJSONResponse() = %v, want %v", got, test.wantJSON)
			}
			gotBody, err := io.ReadAll(resp.RawResponse.Body)
			if err != nil {
				t.Fatalf("read wrapped response body: %v", err)
			}
			if !bytes.Equal(gotBody, test.body) {
				t.Fatalf("wrapped response body changed: got %q, want %q", gotBody, test.body)
			}
			if original.closed {
				t.Fatal("response body closed before wrapper Close")
			}
			if err := resp.RawResponse.Body.Close(); err != nil {
				t.Fatalf("close wrapped response body: %v", err)
			}
			if !original.closed {
				t.Fatal("wrapper Close did not close original response body")
			}
			if test.wantJSON && !isJSONResponse(resp) {
				t.Fatalf("sniffed JSON response was not normalized: %q", resp.RawResponse.Header.Get("Content-Type"))
			}
		})
	}
}

// ==================== captureCodexStreamingResponse ====================

func TestCaptureCodexStreamingResponse_ByteFidelityAndUsage(t *testing.T) {
	cases := []struct {
		name         string
		reasoning    int
		wantDegraded bool
	}{
		{"516 降智", 516, true},
		{"800 正常", 800, false},
		{"0 无推理", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sse := codexSSE(tc.reasoning)
			resp := newCodexStreamResponse(sse)
			attemptLog := &ReqeustLog{}

			status, header, body, err := captureCodexStreamingResponse(resp, attemptLog, time.Now(), context.Background(), 16*1024*1024)
			if err != nil {
				t.Fatalf("capture err: %v", err)
			}
			if status != http.StatusOK {
				t.Fatalf("status=%d want 200", status)
			}
			// 字节保真：缓冲内容必须与上游原始字节完全一致
			if !bytes.Equal(body, sse) {
				t.Fatalf("body not byte-faithful:\n got=%q\nwant=%q", body, sse)
			}
			if attemptLog.ReasoningTokens != tc.reasoning {
				t.Fatalf("ReasoningTokens=%d want %d", attemptLog.ReasoningTokens, tc.reasoning)
			}
			if header.Get("X-Accel-Buffering") != "no" {
				t.Fatalf("X-Accel-Buffering=%q want no", header.Get("X-Accel-Buffering"))
			}
			if (attemptLog.ReasoningTokens == 516) != tc.wantDegraded {
				t.Fatalf("degraded判定 mismatch: reasoning=%d wantDegraded=%v", attemptLog.ReasoningTokens, tc.wantDegraded)
			}
		})
	}
}

func TestCaptureCodexStreamingResponse_TooLarge(t *testing.T) {
	resp := newCodexStreamResponse(codexSSE(516))
	_, _, body, err := captureCodexStreamingResponse(resp, &ReqeustLog{}, time.Now(), context.Background(), 10)
	if !errors.Is(err, errResponseTooLargeToBuffer) {
		t.Fatalf("err=%v want errResponseTooLargeToBuffer", err)
	}
	if len(body) == 0 {
		t.Fatalf("已缓冲内容应随错误返回")
	}
}

func TestCaptureCodexStreamingResponse_TooLargeUsesNamespaceHook(t *testing.T) {
	resp := newCodexStreamResponse(codexNamespaceSSE(516))
	modified := 0
	_, header, body, err := captureCodexStreamingResponse(
		resp,
		&ReqeustLog{},
		time.Now(),
		context.Background(),
		100,
		NewCodexMultiAgentNamespaceSSEHook(&modified),
	)
	if !errors.Is(err, errResponseTooLargeToBuffer) {
		t.Fatalf("err=%v want errResponseTooLargeToBuffer", err)
	}
	if modified == 0 || !bytes.Contains(body, []byte(`"namespace":"collaboration"`)) {
		t.Fatalf("buffered prefix was not namespace-rewritten: modified=%d body=%q", modified, body)
	}
	if header.Get("Content-Length") != "" {
		t.Fatalf("captured streaming response retained Content-Length %q", header.Get("Content-Length"))
	}
}

func TestCaptureCodexStreamingResponse_ClientAbort(t *testing.T) {
	resp := newCodexStreamResponse(codexSSE(516))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err := captureCodexStreamingResponse(resp, &ReqeustLog{}, time.Now(), ctx, 16*1024*1024)
	if !errors.Is(err, errClientAbort) {
		t.Fatalf("err=%v want errClientAbort", err)
	}
}

func TestCaptureCodexStreamingResponse_NoUsage(t *testing.T) {
	// 流被截断、无 response.completed：usage 应为 0，不判降智
	sse := []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
	resp := newCodexStreamResponse(sse)
	attemptLog := &ReqeustLog{}
	_, _, body, err := captureCodexStreamingResponse(resp, attemptLog, time.Now(), context.Background(), 16*1024*1024)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !bytes.Equal(body, sse) {
		t.Fatalf("body not byte-faithful")
	}
	if attemptLog.ReasoningTokens != 0 {
		t.Fatalf("ReasoningTokens=%d want 0", attemptLog.ReasoningTokens)
	}
}

// ==================== captureCodexNonStreamingResponse ====================

func TestCaptureCodexNonStreamingResponse_Usage(t *testing.T) {
	// 非流式 /responses 响应 usage 在顶层（无 response. 前缀）
	body := []byte(`{"id":"resp_1","usage":{"input_tokens":10,"output_tokens":5,"output_tokens_details":{"reasoning_tokens":516}}}`)
	resp := xrequest.NewResponse(&http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	})
	attemptLog := &ReqeustLog{}
	status, _, captured, err := captureCodexNonStreamingResponse(resp, attemptLog)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	if !bytes.Equal(captured, body) {
		t.Fatalf("body mismatch")
	}
	if attemptLog.ReasoningTokens != 516 {
		t.Fatalf("非流式 reasoning 解析失败: got=%d want 516（验证用 ClaudeCodeParse 读顶层 usage.*）", attemptLog.ReasoningTokens)
	}
}

// ==================== writeCapturedResponse ====================

func TestWriteCapturedResponse(t *testing.T) {
	header := http.Header{}
	header.Set("Content-Type", "text/event-stream")
	header.Set("X-Accel-Buffering", "no")
	body := []byte("data: hello\n\n")

	rec := newStreamingRecorder()
	if err := writeCapturedResponse(rec, http.StatusOK, header, body); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.status != http.StatusOK {
		t.Fatalf("status=%d", rec.status)
	}
	if rec.BodyString() != "data: hello\n\n" {
		t.Fatalf("body=%q", rec.BodyString())
	}
	if rec.header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("Content-Type=%q", rec.header.Get("Content-Type"))
	}
}

// ==================== shouldDetectCodexDegradation ====================

func newDegradationAppSettings(t *testing.T, settings AppSettings) *AppSettingsService {
	t.Helper()
	as := &AppSettingsService{path: filepath.Join(t.TempDir(), "app.json")}
	if _, err := as.SaveAppSettings(settings); err != nil {
		t.Fatalf("SaveAppSettings: %v", err)
	}
	return as
}

func TestShouldDetectCodexDegradation(t *testing.T) {
	as := newDegradationAppSettings(t, AppSettings{
		CodexDegradationResendEnabled:   true,
		CodexDegradationMaxResend:       3,
		CodexDegradationReasoningTokens: []int{516},
	})
	cases := []struct {
		kind, endpoint string
		want           bool
	}{
		{"codex", "/responses", true},
		{"codex", "/v1/chat/completions", false}, // 非 /responses 入口
		{"codex", "/responses/count_tokens", false},
		{"claude", "/responses", false},
		{"codex", "/anything", false},
	}
	for _, c := range cases {
		got := shouldDetectCodexDegradation(as, c.kind, c.endpoint)
		if got != c.want {
			t.Errorf("shouldDetectCodexDegradation(%q,%q)=%v want %v", c.kind, c.endpoint, got, c.want)
		}
	}

	// 关闭开关
	disabled := newDegradationAppSettings(t, AppSettings{
		CodexDegradationResendEnabled:   false,
		CodexDegradationMaxResend:       3,
		CodexDegradationReasoningTokens: []int{516},
	})
	if shouldDetectCodexDegradation(disabled, "codex", "/responses") {
		t.Errorf("关闭开关时应返回 false")
	}

	// 特征值集合为空应视为关闭
	emptyTokens := newDegradationAppSettings(t, AppSettings{
		CodexDegradationResendEnabled:   true,
		CodexDegradationMaxResend:       3,
		CodexDegradationReasoningTokens: nil,
	})
	if shouldDetectCodexDegradation(emptyTokens, "codex", "/responses") {
		t.Errorf("特征值集合为空时应返回 false")
	}
}

// ==================== forwardCodexWithDegradationRetry 集成测试 ====================

func newDegradationTestRelay(t *testing.T, maxResend int, tokens []int) (*ProviderRelayService, *AppSettingsService) {
	t.Helper()
	as := newDegradationAppSettings(t, AppSettings{
		CodexDegradationResendEnabled:   true,
		CodexDegradationMaxResend:       maxResend,
		CodexDegradationReasoningTokens: tokens,
	})
	return &ProviderRelayService{
		httpClient:  newRelayHTTPClient(),
		appSettings: as,
	}, as
}

// runDegradationRetry 起一个按调用次数返回不同 reasoning 的假上游，跑一次重发循环。
func runDegradationRetry(t *testing.T, prs *ProviderRelayService, reasoningPerCall []int, rewriteNamespace ...bool) (callCount int, rec *httptest.ResponseRecorder) {
	t.Helper()
	var mu sync.Mutex
	idx := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := idx
		if n >= len(reasoningPerCall) {
			n = len(reasoningPerCall) - 1
		}
		reasoning := reasoningPerCall[n]
		idx++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		responseBody := codexSSE(reasoning)
		if len(rewriteNamespace) > 0 && rewriteNamespace[0] {
			responseBody = codexNamespaceSSE(reasoning)
		}
		_, _ = w.Write(responseBody)
	}))
	defer server.Close()

	gin.SetMode(gin.TestMode)
	rec = httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5-codex","input":"hi","stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(body))

	provider := Provider{APIURL: server.URL, ConnectivityAuthType: "bearer"}
	headers := map[string]string{"Content-Type": "application/json"}
	ok, err := prs.forwardCodexWithDegradationRetry(c, provider, "/responses", nil, headers, body, true, "gpt-5-codex", rewriteNamespace...)
	if err != nil {
		t.Fatalf("forwardCodexWithDegradationRetry err: %v", err)
	}
	if !ok {
		t.Fatalf("want ok=true")
	}
	mu.Lock()
	callCount = idx
	mu.Unlock()
	return callCount, rec
}

func TestForwardCodexWithDegradationRetry_RetriesOnDegraded(t *testing.T) {
	prs, _ := newDegradationTestRelay(t, 3, []int{516})
	callCount, rec := runDegradationRetry(t, prs, []int{516, 800})
	if callCount != 2 {
		t.Fatalf("上游调用次数=%d want 2（首次降智重发，二次正常）", callCount)
	}
	if !strings.Contains(rec.Body.String(), `"reasoning_tokens":800`) {
		t.Fatalf("客户端最终应收到未降智(800)结果, got %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"reasoning_tokens":516`) {
		t.Fatalf("客户端不应收到降智(516)结果, got %q", rec.Body.String())
	}
}

func TestForwardCodexWithDegradationRetry_AllDegradedReturnsLast(t *testing.T) {
	prs, _ := newDegradationTestRelay(t, 1, []int{516})
	callCount, rec := runDegradationRetry(t, prs, []int{516, 516, 516})
	// maxResend=1 => 最多 2 次请求（首次 + 1 次重发），最后仍降智则返回降智结果
	if callCount != 2 {
		t.Fatalf("上游调用次数=%d want 2", callCount)
	}
	if !strings.Contains(rec.Body.String(), `"reasoning_tokens":516`) {
		t.Fatalf("全部降智时应返回最后一次(516)结果, got %q", rec.Body.String())
	}
}

func TestForwardCodexWithDegradationRetry_MultipleTokenValues(t *testing.T) {
	// 特征值集合 [516, 1030]：1030 也应触发重发，最终拿到未降智结果
	prs, _ := newDegradationTestRelay(t, 3, []int{516, 1030})
	callCount, rec := runDegradationRetry(t, prs, []int{1030, 800})
	if callCount != 2 {
		t.Fatalf("上游调用次数=%d want 2（首次 1030 命中特征值应重发）", callCount)
	}
	if !strings.Contains(rec.Body.String(), `"reasoning_tokens":800`) {
		t.Fatalf("客户端最终应收到未降智(800)结果, got %q", rec.Body.String())
	}
}

func TestForwardCodexWithDegradationRetry_NoDegradedFirstTry(t *testing.T) {
	prs, _ := newDegradationTestRelay(t, 3, []int{516})
	callCount, _ := runDegradationRetry(t, prs, []int{800})
	if callCount != 1 {
		t.Fatalf("首次未降智应只调用 1 次, got %d", callCount)
	}
}

func TestForwardCodexWithDegradationRetry_RewritesFinalNamespace(t *testing.T) {
	prs, _ := newDegradationTestRelay(t, 3, []int{516})
	callCount, rec := runDegradationRetry(t, prs, []int{516, 800}, true)
	if callCount != 2 {
		t.Fatalf("upstream calls = %d, want 2", callCount)
	}
	response := rec.Body.String()
	if strings.Contains(response, `"namespace":"agents"`) {
		t.Fatalf("final response retained agents namespace: %s", response)
	}
	if count := strings.Count(response, `"namespace":"collaboration"`); count != 1 {
		t.Fatalf("final namespace count = %d, response = %s", count, response)
	}
	if strings.Contains(response, `"reasoning_tokens":516`) || !strings.Contains(response, `"reasoning_tokens":800`) {
		t.Fatalf("client did not receive only the final non-degraded response: %s", response)
	}
}

func TestForwardCodexWithDegradationRetry_RewritesAllDegradedResult(t *testing.T) {
	prs, _ := newDegradationTestRelay(t, 1, []int{516})
	callCount, rec := runDegradationRetry(t, prs, []int{516, 516}, true)
	if callCount != 2 {
		t.Fatalf("upstream calls = %d, want 2", callCount)
	}
	response := rec.Body.String()
	if strings.Contains(response, `"namespace":"agents"`) || !strings.Contains(response, `"namespace":"collaboration"`) {
		t.Fatalf("last degraded response namespace was not rewritten: %s", response)
	}
	if !strings.Contains(response, `"reasoning_tokens":516`) {
		t.Fatalf("last degraded response was not returned: %s", response)
	}
}

func TestForwardCodexWithDegradationRetry_RewritesNonStreamingNamespace(t *testing.T) {
	for _, test := range []struct {
		name        string
		contentType string
	}{
		{name: "json content type", contentType: "application/json"},
		{name: "missing content type"},
		{name: "incorrect SSE content type", contentType: "text/event-stream"},
	} {
		t.Run(test.name, func(t *testing.T) {
			responseBody := []byte(`{"output":[{"type":"custom_tool_call","namespace":"agents","name":"send_message"}],"usage":{"input_tokens":1,"output_tokens":1,"output_tokens_details":{"reasoning_tokens":800}}}`)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.contentType != "" {
					w.Header().Set("Content-Type", test.contentType)
				}
				_, _ = w.Write(responseBody)
			}))
			defer server.Close()

			prs, _ := newDegradationTestRelay(t, 1, []int{516})
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			body := []byte(`{"model":"gpt-5-codex","input":"hi","stream":true}`)
			c.Request = httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(body))
			provider := Provider{Name: "degradation-non-stream", APIURL: server.URL}
			ok, err := prs.forwardCodexWithDegradationRetry(c, provider, "/responses", nil, map[string]string{"Content-Type": "application/json"}, body, true, "gpt-5-codex", true)
			if err != nil || !ok {
				t.Fatalf("forwardCodexWithDegradationRetry() = (%v, %v)", ok, err)
			}
			if got := gjson.Get(rec.Body.String(), "output.0.namespace").String(); got != "collaboration" {
				t.Fatalf("client response namespace = %q, body = %s", got, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Length"); got != "" {
				t.Fatalf("rewritten buffered response retained Content-Length %q", got)
			}
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
				t.Fatalf("rewritten buffered response content type = %q", got)
			}
		})
	}
}
