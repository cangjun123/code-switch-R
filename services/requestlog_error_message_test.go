package services

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daodao97/xgo/xdb"
	"github.com/gin-gonic/gin"
)

// TestForwardRequestRecordsErrorMessage 验证上游返回 500 时，
// request_log 的 error_message 列记录了上游错误体摘要。
func TestForwardRequestRecordsErrorMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayTestEnv(t)
	if GlobalDBQueueLogs == nil {
		if err := InitGlobalDBQueue(); err != nil {
			t.Fatalf("初始化数据库队列失败: %v", err)
		}
	}

	db, err := xdb.DB("default")
	if err != nil {
		t.Fatalf("获取数据库连接失败: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM request_log WHERE provider = 'err-msg-provider'`); err != nil {
		t.Fatalf("清理 request_log 失败: %v", err)
	}

	// 模拟上游：返回 500 + JSON 错误体
	upstreamBody := `{"error":{"type":"server_error","message":"upstream exploded"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	prs := &ProviderRelayService{httpClient: newRelayHTTPClient()}
	provider := Provider{
		Name:   "err-msg-provider",
		APIURL: upstream.URL,
		APIKey: "sk-test",
	}

	body := []byte(`{"model":"claude-opus-4","stream":false,"messages":[{"role":"user","content":"hi"}],"max_tokens":10}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))

	ok, err := prs.forwardRequest(c, "claude", provider, "/v1/messages", nil, map[string]string{"Content-Type": "application/json"}, body, false, "claude-opus-4")
	if ok {
		t.Fatal("forwardRequest 应返回 ok=false（上游 500）")
	}
	if err == nil {
		t.Fatal("forwardRequest 应返回错误")
	}

	// request_log 写入经过批量队列（100ms tick），轮询等待落库
	deadline := time.Now().Add(5 * time.Second)
	var httpCode int
	var errorMessage sql.NullString
	for {
		rowErr := db.QueryRow(`
			SELECT http_code, error_message FROM request_log
			WHERE provider = 'err-msg-provider' ORDER BY id DESC LIMIT 1
		`).Scan(&httpCode, &errorMessage)
		if rowErr == nil {
			break
		}
		if !strings.Contains(rowErr.Error(), "no rows") {
			t.Fatalf("查询 request_log 失败: %v", rowErr)
		}
		if time.Now().After(deadline) {
			t.Fatal("等待 request_log 落库超时")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if httpCode != http.StatusInternalServerError {
		t.Fatalf("http_code = %d, want 500", httpCode)
	}
	if !errorMessage.Valid || errorMessage.String == "" {
		t.Fatal("error_message 应记录上游错误体")
	}
	if !strings.Contains(errorMessage.String, "upstream exploded") {
		t.Fatalf("error_message 应包含上游错误信息，实际: %s", errorMessage.String)
	}
}

// TestForwardRequestRecordsTransportError 验证连接失败（传输错误）时
// error_message 记录传输错误摘要。
func TestForwardRequestRecordsTransportError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayTestEnv(t)
	if GlobalDBQueueLogs == nil {
		if err := InitGlobalDBQueue(); err != nil {
			t.Fatalf("初始化数据库队列失败: %v", err)
		}
	}

	db, err := xdb.DB("default")
	if err != nil {
		t.Fatalf("获取数据库连接失败: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM request_log WHERE provider = 'err-transport-provider'`); err != nil {
		t.Fatalf("清理 request_log 失败: %v", err)
	}

	// 启动后立刻关闭，得到一个必然连接失败的地址
	deadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := deadServer.URL
	deadServer.Close()

	prs := &ProviderRelayService{httpClient: newRelayHTTPClient()}
	provider := Provider{
		Name:   "err-transport-provider",
		APIURL: deadURL,
		APIKey: "sk-test",
	}

	body := []byte(`{"model":"claude-opus-4","stream":false,"messages":[{"role":"user","content":"hi"}],"max_tokens":10}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))

	ok, err := prs.forwardRequest(c, "claude", provider, "/v1/messages", nil, map[string]string{"Content-Type": "application/json"}, body, false, "claude-opus-4")
	if ok || err == nil {
		t.Fatalf("forwardRequest 应失败，实际 (%v, %v)", ok, err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var errorMessage sql.NullString
	for {
		rowErr := db.QueryRow(`
			SELECT error_message FROM request_log
			WHERE provider = 'err-transport-provider' ORDER BY id DESC LIMIT 1
		`).Scan(&errorMessage)
		if rowErr == nil {
			break
		}
		if !strings.Contains(rowErr.Error(), "no rows") {
			t.Fatalf("查询 request_log 失败: %v", rowErr)
		}
		if time.Now().After(deadline) {
			t.Fatal("等待 request_log 落库超时")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !errorMessage.Valid || errorMessage.String == "" {
		t.Fatal("传输错误应记录 error_message")
	}
}

// TestTruncateErrorMessage 验证错误摘要截断逻辑。
func TestTruncateErrorMessage(t *testing.T) {
	if got := truncateErrorMessage("short"); got != "short" {
		t.Fatalf("短消息不应截断，实际: %q", got)
	}
	if got := truncateErrorMessage("   padded   "); got != "padded" {
		t.Fatalf("应去除首尾空白，实际: %q", got)
	}
	long := strings.Repeat("x", 600)
	got := truncateErrorMessage(long)
	if len(got) != requestLogErrorMessageMaxBytes+3 {
		t.Fatalf("长消息应截断到 %d 字节+省略号，实际 %d", requestLogErrorMessageMaxBytes, len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("截断消息应以省略号结尾，实际: %q", got[len(got)-10:])
	}
}

// TestSetRequestLogErrorKeepsFirstError 验证只保留首个错误。
func TestSetRequestLogErrorKeepsFirstError(t *testing.T) {
	log := &ReqeustLog{}
	setRequestLogError(log, "first error")
	setRequestLogError(log, "second error")
	if log.ErrorMessage != "first error" {
		t.Fatalf("应保留首个错误，实际: %q", log.ErrorMessage)
	}
	// 空消息不应覆盖
	setRequestLogError(log, "")
	if log.ErrorMessage != "first error" {
		t.Fatalf("空消息不应覆盖，实际: %q", log.ErrorMessage)
	}
	// nil 安全
	setRequestLogError(nil, "boom")
}
