package services

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"errors"
	"testing"

	"github.com/gin-gonic/gin"
)

// 端到端：claude kind + openai_chat 上游 + 协议转换，观察 Claude Code 实际收到的字节流。
// 场景 A：正常完整流。场景 B：逐词正常推送但流式响应慢（chunked，分多次 flush）。
func TestE2EProtocolConversionStream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	words := []string{"Hello!", " 👋", " I'm", " ready", " to", " help", "."}

	// 模拟真实上游：SSE 逐 chunk flush（每个 data 行独立 Write+Flush，中间隔空行）
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		// 首个 chunk：role + reasoning
		fmt.Fprintf(w, "data: %s\n\n", `{"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"hmm"},"finish_reason":null}]}`)
		flusher.Flush()
		for _, word := range words {
			fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(`{"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`, `"`+strings.ReplaceAll(word, `"`, `\"`)+`"`))
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: %s\n\n", `{"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		flusher.Flush()
		fmt.Fprintf(w, "data: %s\n\n", `{"id":"c1","model":"glm-5.2","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":80}}}`)
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	prs := &ProviderRelayService{httpClient: newRelayHTTPClient()}
	provider := Provider{
		Name:            "e2e-openai-chat",
		APIURL:          server.URL,
		APIKey:          "sk-test",
		UpstreamProtocol: "openai_chat",
		APIEndpoint:     "/chat/completions",
	}

	body := []byte(`{"model":"claude-opus-4","stream":true,"system":"sys","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))

	ok, err := prs.forwardRequest(c, "claude", provider, "/v1/messages", nil, map[string]string{"Content-Type": "application/json"}, body, true, "claude-opus-4")
	if !ok || err != nil {
		t.Fatalf("forwardRequest = (%v, %v)", ok, err)
	}

	clientStream := rec.Body.String()
	t.Logf("=== 客户端收到的字节流（%d 字节）===\n%.1200s", len(clientStream), clientStream)

	// 关键断言 1：块数。thinking+text 至多 2 个 content_block_start
	starts := strings.Count(clientStream, `"type":"content_block_start"`)
	t.Logf("content_block_start 数量: %d", starts)
	if starts > 2 {
		t.Errorf("碎块！content_block_start=%d > 2", starts)
	}

	// 关键断言 2：SSE 拓扑完整性——event 行后必须紧跟 data 行
	eventLines := 0
	brokenEvents := 0
	for _, line := range strings.Split(clientStream, "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if strings.HasPrefix(trimmed, "event: ") {
			eventLines++
		}
	}
	// 每个 event 行配一个 data 行
	dataLines := strings.Count(clientStream, "\ndata: ") + strings.Count(clientStream, "\r\ndata: ")
	t.Logf("event 行=%d data 行=%d", eventLines, dataLines)
	if eventLines != dataLines {
		brokenEvents++
		t.Errorf("SSE 拓扑破损：event=%d data=%d", eventLines, dataLines)
	}

	// 关键断言 3：块 index 单调性
	var indexes []int
	rest := clientStream
	for {
		pos := strings.Index(rest, `"type":"content_block_start"`)
		if pos < 0 {
			break
		}
		seg := rest[pos:]
		ipos := strings.Index(seg, `"index":`)
		seg2 := seg[ipos+8:]
		end := strings.IndexAny(seg2, ",}")
		var idx int
		fmt.Sscanf(strings.TrimSpace(seg2[:end]), "%d", &idx)
		indexes = append(indexes, idx)
		rest = seg[pos+10:]
	}
	t.Logf("块 index 序列: %v", indexes)
	for i := 1; i < len(indexes); i++ {
		if indexes[i] <= indexes[i-1] {
			t.Errorf("index 回跳: %v", indexes)
		}
	}
}

// 场景 B：上游逐词推流后连接被掐断（无 finish/无 usage/无 DONE）。
// 部分响应已透传给客户端无法重发，但必须返回截断哨兵让外层记 provider 失败，
// 否则客户端（Claude Code 报 stream disconnected）自行重试时路由回同一个坏
// provider，反复拿到断流碎片（表现为逐词碎块 + usage 永远为 0）。
func TestE2ETruncatedConversionStream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, word := range []string{"Hello!", " 👋", " I'm"} {
			fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(`{"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`, `"`+word+`"`))
			flusher.Flush()
		}
		// 模拟上游崩溃：panic 触发连接重置
		panic("simulated upstream crash mid-stream")
	}))
	defer server.Close()

	prs := &ProviderRelayService{httpClient: newRelayHTTPClient()}
	provider := Provider{
		Name:            "e2e-crash",
		APIURL:          server.URL,
		APIKey:          "sk-test",
		UpstreamProtocol: "openai_chat",
		APIEndpoint:     "/chat/completions",
	}

	body := []byte(`{"model":"claude-opus-4","stream":true,"messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))

	ok, err := prs.forwardRequest(c, "claude", provider, "/v1/messages", nil, map[string]string{"Content-Type": "application/json"}, body, true, "claude-opus-4")

	// 部分响应已透传（客户端拿到了 3 个词），ok 仍应为 true
	if !ok {
		t.Fatalf("ok=false（部分响应已透传应仍为 ok=true）, err=%v", err)
	}
	// 必须返回截断哨兵，让外层 proxyHandler 记 provider 失败
	if !errors.Is(err, errStreamTruncated) {
		t.Fatalf("截断流应返回 errStreamTruncated, got %v", err)
	}
	// 客户端确实收到了部分内容
	if !strings.Contains(rec.Body.String(), "Hello!") {
		t.Fatal("客户端应收到已透传的部分内容")
	}
}
