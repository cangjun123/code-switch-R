package services

// Codex 请求链路追踪：定位"客户端等了 100s 但上游只处理了 10s"这类差值
// 消耗在本进程哪个阶段（认证、quota 检查、调度、上游连接/上传、预检、
// 首 token 写回、结算）。只记录耗时、大小和状态码，不记录请求内容。
//
// 开关：app.json 的 codex_trace_enabled（默认关闭），或环境变量
// CODESWITCH_CODEX_TRACE=1 强制开启。开启后每个 Codex 请求结束打一条
// 汇总行；总耗时超过 codexTraceSlowThreshold 的慢请求追加分段明细。

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net/http/httptrace"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const codexTraceSlowThreshold = 10 * time.Second

// codexTraceAttemptTiming collects one upstream attempt's transport phases.
// Callbacks may fire on different goroutines, so access is mutex-guarded.
type codexTraceAttemptTiming struct {
	mu           sync.Mutex
	attemptStart time.Time
	attemptID    string

	dnsDone     time.Time
	connectDone time.Time
	tlsDone     time.Time
	wroteReq    time.Time
	firstByte   time.Time

	hasDNS     bool
	hasConnect bool
	hasTLS     bool
	hasWrite   bool
}

type codexTraceAttempt struct {
	attemptID string
	dnsMs     float64 // -1 = phase not observed (e.g. reused connection)
	connectMs float64
	tlsMs     float64
	writeMs   float64
	headersMs float64
}

type codexTraceStage struct {
	name      string
	elapsedMs float64
}

type codexTrace struct {
	mu         sync.Mutex
	requestID  string
	keyID      string
	model      string
	provider   string
	start      time.Time
	stages     []codexTraceStage
	attempts   []codexTraceAttempt
	bodyBytes  int64
	httpStatus int
}

type codexTraceContextKey struct{}

func codexTraceFromContext(ctx context.Context) *codexTrace {
	if ctx == nil {
		return nil
	}
	trace, _ := ctx.Value(codexTraceContextKey{}).(*codexTrace)
	return trace
}

func codexTraceRequestID(ctx context.Context) string {
	if trace := codexTraceFromContext(ctx); trace != nil {
		return trace.requestID
	}
	return ""
}

// markCodexPreflightFirstEvent records the first upstream SSE event seen by a
// preflight inspector so slow first-token requests can attribute the wait.
func markCodexPreflightFirstEvent(ctx context.Context) {
	codexTraceFromContext(ctx).markOnce("first_upstream_event")
}

func newCodexTraceID() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return time.Now().Format("150405.000")
	}
	return hex.EncodeToString(buf)
}

func (t *codexTrace) mark(name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stages = append(t.stages, codexTraceStage{name: name, elapsedMs: msSince(t.start)})
}

// markOnce records a stage only the first time it fires (first token, settle).
func (t *codexTrace) markOnce(name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, stage := range t.stages {
		if stage.name == name {
			return
		}
	}
	t.stages = append(t.stages, codexTraceStage{name: name, elapsedMs: msSince(t.start)})
}

func (t *codexTrace) setKeyID(keyID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.keyID = keyID
	t.mu.Unlock()
}

func (t *codexTrace) setModel(model string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.model = model
	t.mu.Unlock()
}

func (t *codexTrace) setProvider(provider string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.provider = provider
	t.mu.Unlock()
}

func (t *codexTrace) setBodyBytes(size int64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.bodyBytes = size
	t.mu.Unlock()
}

func (t *codexTrace) setStatus(status int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.httpStatus = status
	t.mu.Unlock()
}

func (t *codexTrace) recordAttempt(attempt codexTraceAttempt) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.attempts) >= 16 {
		t.attempts = t.attempts[1:]
	}
	t.attempts = append(t.attempts, attempt)
}

// elapsedOf returns the last recorded elapsed for a stage, -1 if absent.
func (t *codexTrace) elapsedOf(name string) float64 {
	if t == nil {
		return -1
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	result := float64(-1)
	for _, stage := range t.stages {
		if stage.name == name {
			result = stage.elapsedMs
		}
	}
	return result
}

// spanOf returns the duration between two stages (last occurrence), -1 if
// either stage is missing.
func (t *codexTrace) spanOf(fromName, toName string) float64 {
	if t == nil {
		return -1
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	from, to := float64(-1), float64(-1)
	for _, stage := range t.stages {
		switch stage.name {
		case fromName:
			from = stage.elapsedMs
		case toName:
			to = stage.elapsedMs
		}
	}
	if from < 0 || to < 0 {
		return -1
	}
	return to - from
}

func (t *codexTrace) lastAttempt() (codexTraceAttempt, bool) {
	if t == nil {
		return codexTraceAttempt{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.attempts) == 0 {
		return codexTraceAttempt{}, false
	}
	return t.attempts[len(t.attempts)-1], true
}

func (t *codexTrace) finish() {
	if t == nil {
		return
	}
	totalMs := msSince(t.start)
	quotaWait := t.spanOf("quota_check_start", "quota_check_done")
	firstToken := t.elapsedOf("first_client_write")
	lastAttempt, hasAttempt := t.lastAttempt()

	summary := fmt.Sprintf(
		"[Codex Trace] req=%s key=%s model=%s provider=%s status=%d attempts=%d body_kb=%.1f auth_ms=%s quota_wait_ms=%s dispatch_ms=%s",
		t.requestID, t.keyID, t.model, t.provider, t.httpStatus, func() int {
			t.mu.Lock()
			defer t.mu.Unlock()
			return len(t.attempts)
		}(),
		float64(t.bodyBytes)/1024,
		formatTraceMs(t.elapsedOf("auth_done")),
		formatTraceMs(quotaWait),
		formatTraceMs(t.elapsedOf("dispatch")),
	)
	if hasAttempt {
		summary += fmt.Sprintf(" upstream_write_ms=%s upstream_headers_ms=%s",
			formatTraceMs(lastAttempt.writeMs), formatTraceMs(lastAttempt.headersMs))
	}
	summary += fmt.Sprintf(" first_token_ms=%s settle_at_ms=%s total_ms=%.0f",
		formatTraceMs(firstToken), formatTraceMs(t.elapsedOf("settle")), totalMs)
	fmt.Println(summary)

	if totalMs < float64(codexTraceSlowThreshold/time.Millisecond) {
		return
	}
	t.mu.Lock()
	stages := append([]codexTraceStage(nil), t.stages...)
	attempts := append([]codexTraceAttempt(nil), t.attempts...)
	t.mu.Unlock()
	for _, stage := range stages {
		fmt.Printf("[Codex Trace] req=%s stage=%s at_ms=%.1f\n", t.requestID, stage.name, stage.elapsedMs)
	}
	for _, attempt := range attempts {
		fmt.Printf("[Codex Trace] req=%s attempt=%s dns_ms=%s connect_ms=%s tls_ms=%s write_ms=%s headers_ms=%s\n",
			t.requestID, attempt.attemptID,
			formatTraceMs(attempt.dnsMs), formatTraceMs(attempt.connectMs), formatTraceMs(attempt.tlsMs),
			formatTraceMs(attempt.writeMs), formatTraceMs(attempt.headersMs))
	}
}

func formatTraceMs(value float64) string {
	if value < 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f", value)
}

func msSince(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000
}

// codexTraceEnabled resolves the trace switch: the env var forces tracing on;
// otherwise the persisted setting is consulted through an mtime+size cache so
// the hot path does not re-read app.json per request.
func codexTraceEnabled(as *AppSettingsService) bool {
	if envTraceFlag() {
		return true
	}
	return as != nil && as.IsCodexTraceEnabled()
}

func envTraceFlag() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODESWITCH_CODEX_TRACE"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// codexTraceMiddleware must be the first middleware on Codex inference routes
// so every later stage (auth, quota, dispatch) lands inside the trace window.
func (prs *ProviderRelayService) codexTraceMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !codexTraceEnabled(prs.appSettings) {
			c.Next()
			return
		}
		trace := &codexTrace{requestID: newCodexTraceID(), start: time.Now()}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), codexTraceContextKey{}, trace))
		if keyID := relayKeyIDFromContext(c); keyID != "" {
			trace.setKeyID(keyID)
		}
		defer trace.finish()
		c.Next()
	}
}

// withCodexTraceHTTPTracing attaches httptrace to one upstream attempt so the
// transport phases (DNS / TCP / TLS / request-body write / response headers)
// are separated inside what upstream_headers_ms used to measure as one block.
func withCodexTraceHTTPTracing(ctx context.Context, trace *codexTrace, attemptID string) context.Context {
	if trace == nil {
		return ctx
	}
	timing := &codexTraceAttemptTiming{attemptStart: time.Now(), attemptID: attemptID}
	record := func() {
		timing.mu.Lock()
		attempt := codexTraceAttempt{attemptID: timing.attemptID,
			dnsMs: -1, connectMs: -1, tlsMs: -1, writeMs: -1, headersMs: msSince(timing.attemptStart)}
		if timing.hasDNS {
			attempt.dnsMs = msUntil(timing.attemptStart, timing.dnsDone)
		}
		if timing.hasConnect {
			attempt.connectMs = msUntil(timing.attemptStart, timing.connectDone)
		}
		if timing.hasTLS {
			attempt.tlsMs = msUntil(timing.attemptStart, timing.tlsDone)
		}
		if timing.hasWrite {
			attempt.writeMs = msUntil(timing.attemptStart, timing.wroteReq)
		}
		timing.mu.Unlock()
		trace.recordAttempt(attempt)
	}
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		DNSDone: func(httptrace.DNSDoneInfo) {
			timing.mu.Lock()
			timing.dnsDone = time.Now()
			timing.hasDNS = true
			timing.mu.Unlock()
		},
		ConnectDone: func(network, addr string, err error) {
			timing.mu.Lock()
			timing.connectDone = time.Now()
			timing.hasConnect = true
			timing.mu.Unlock()
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			timing.mu.Lock()
			timing.tlsDone = time.Now()
			timing.hasTLS = true
			timing.mu.Unlock()
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			timing.mu.Lock()
			timing.wroteReq = time.Now()
			timing.hasWrite = true
			timing.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			record()
		},
	})
}

func msUntil(start, end time.Time) float64 {
	return float64(end.Sub(start).Microseconds()) / 1000
}
