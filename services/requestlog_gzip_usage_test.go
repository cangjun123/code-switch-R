package services

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daodao97/xgo/xdb"
	"github.com/gin-gonic/gin"
)

// TestForwardRequestCountsUsageFromGzipSSE 复现 CLIProxyAPI 场景：客户端（Claude Code）发送
// Accept-Encoding: gzip，上游据此 gzip 压缩 SSE。relay 必须让 transport 透明解压，
// 否则用量钩子只能看到压缩字节，输入/输出 token 全部记为 0。
func TestForwardRequestCountsUsageFromGzipSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayTestEnv(t)
	if GlobalDBQueueLogs == nil {
		if err := InitGlobalDBQueue(); err != nil {
			t.Fatal(err)
		}
	}
	db, err := xdb.DB("default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM request_log WHERE provider = 'gzip-sse-provider'`); err != nil {
		t.Fatal(err)
	}

	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-4\",\"usage\":{\"input_tokens\":12,\"output_tokens\":1}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":34}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = gz.Write([]byte(sse))
			_ = gz.Close()
			return
		}
		_, _ = w.Write([]byte(sse))
	}))
	defer upstream.Close()

	prs := &ProviderRelayService{httpClient: newRelayHTTPClient()}
	provider := Provider{Name: "gzip-sse-provider", APIURL: upstream.URL, APIKey: "sk-test"}
	body := []byte(`{"model":"claude-opus-4","stream":true,"messages":[{"role":"user","content":"hi"}],"max_tokens":10}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	headers := map[string]string{"Content-Type": "application/json", "Accept-Encoding": "gzip, deflate, br"}

	ok, err := prs.forwardRequest(c, "claude", provider, "/v1/messages", nil, headers, body, true, "claude-opus-4")
	if !ok || err != nil {
		t.Fatalf("forwardRequest: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(rec.Body.String(), "message_start") {
		t.Fatalf("client should receive decompressed SSE, got %q", rec.Body.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		var input, output int
		rowErr := db.QueryRow(`SELECT input_tokens, output_tokens FROM request_log WHERE provider = 'gzip-sse-provider' ORDER BY id DESC LIMIT 1`).Scan(&input, &output)
		if rowErr == nil {
			if input != 12 || output != 34 {
				t.Fatalf("usage not parsed from gzip SSE: input=%d output=%d", input, output)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("request_log not written: %v", rowErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
