package services

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

var (
	fragA      string // 残片A：name 非空 + args 空
	fragB      string // 残片B：name 空 + args 完整
	completeFC string // 完整 functionCall（并行第二调用形态）
	terminal   string // 终止事件
	fragUsage  = map[string]any{"promptTokenCount": 12, "candidatesTokenCount": 0, "totalTokenCount": 12}
	fragArgs   = map[string]any{
		"CommandLine":       "ls -la",
		"Cwd":               ".",
		"WaitMsBeforeAsync": "5000",
		"toolSummary":       "list",
		"toolAction":        "list files",
	}
)

func init() {
	fragA = mustEvent(map[string]any{"name": "run_command", "args": map[string]any{}}, nil, fragUsage)
	fragB = mustEvent(map[string]any{"name": "", "args": fragArgs}, nil, fragUsage)
	completeFC = mustEvent(map[string]any{"name": "view_file", "args": map[string]any{
		"AbsolutePath": "C:/b.txt", "toolSummary": "view", "toolAction": "view",
	}}, nil, map[string]any{"promptTokenCount": 12, "candidatesTokenCount": 5, "totalTokenCount": 17})
	terminal = mustEvent(nil, "STOP", fragUsage)
}

func mustEvent(functionCall map[string]any, finishReason any, usage map[string]any) string {
	var part any
	if functionCall != nil {
		part = map[string]any{"functionCall": functionCall}
	}
	payload := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": []any{part}},
			"finishReason": finishReason,
			"index":        0,
		}},
		"usageMetadata": usage,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return "data: " + string(b) + "\n\n"
}

// buildGeminiTextEvent 构造纯文本事件
func buildGeminiTextEvent(text string) string {
	payload := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": text}}},
			"finishReason": nil,
			"index":        0,
		}},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return "data: " + string(b) + "\n\n"
}

// runStitcher 把 SSE 事件字节流（可任意切分模拟 TCP 分包）喂给 stitcher，
// 返回写给客户端的全部字节。lineBuf 跨 chunk 保持（模拟真实流的连续缓冲）。
func runStitcher(events string, splitAt []int) string {
	stitcher := &geminiFunctionCallStitcher{}
	var out strings.Builder
	var lineBuf strings.Builder

	chunks := splitBytesAt(events, splitAt)
	for _, chunk := range chunks {
		for _, ev := range extractGeminiSSEEvents(chunk, &lineBuf) {
			if got := stitcher.process(ev); got != "" {
				out.WriteString(got)
			}
		}
	}
	// 模拟流结束：放出扣留内容 + 残留
	if pending := stitcher.flushPending(); pending != "" {
		out.WriteString(pending)
	}
	if lineBuf.Len() > 0 {
		out.WriteString(stitcher.processResidual(lineBuf.String()))
	}
	return out.String()
}

// splitBytesAt 按给定位置切分字符串（位置越界时忽略）
func splitBytesAt(s string, at []int) []string {
	if len(at) == 0 {
		return []string{s}
	}
	var chunks []string
	prev := 0
	for _, pos := range at {
		if pos > prev && pos <= len(s) {
			chunks = append(chunks, s[prev:pos])
			prev = pos
		}
	}
	if prev < len(s) {
		chunks = append(chunks, s[prev:])
	}
	return chunks
}

// TestStitcherMergesFragments 残片A+残片B → 合并为一个完整 functionCall 事件
func TestStitcherMergesFragments(t *testing.T) {
	out := runStitcher(fragA+fragB+terminal, nil)

	if got := strings.Count(out, "data:"); got != 2 {
		t.Fatalf("期望合并后只剩 2 个事件（合并fc + 终止），实际 %d 个:\n%s", got, out)
	}
	if !strings.Contains(out, `"name":"run_command"`) {
		t.Fatalf("合并事件应包含 name=run_command:\n%s", out)
	}
	if !strings.Contains(out, `"CommandLine":"ls -la"`) {
		t.Fatalf("合并事件应包含完整 args:\n%s", out)
	}
	// 残片B 的空 name 不应残留
	if strings.Contains(out, `"name":""`) {
		t.Fatalf("合并事件不应残留空 name:\n%s", out)
	}
	// name 和 args 应在同一个事件里
	eventName := strings.Index(out, "run_command")
	eventArgs := strings.Index(out, "CommandLine")
	if eventName == -1 || eventArgs == -1 {
		t.Fatalf("合并事件应同时包含 name 与 args:\n%s", out)
	}
	nextEventSep := strings.Index(out[eventName:], "\n\n")
	if eventArgs > eventName+nextEventSep {
		t.Fatalf("name 与 args 应在同一个事件:\n%s", out)
	}
}

// TestStitcherMergeAcrossChunkBoundary 残片A 和 B 被切在不同 chunk → 仍能合并
func TestStitcherMergeAcrossChunkBoundary(t *testing.T) {
	stream := fragA + fragB + terminal
	// 在残片A 的中间、残片B 的中间各切一刀
	out := runStitcher(stream, []int{60, len(fragA) + 80})

	if !strings.Contains(out, `"name":"run_command"`) || !strings.Contains(out, `"CommandLine":"ls -la"`) {
		t.Fatalf("跨 chunk 切分后仍应合并:\n%s", out)
	}
	if strings.Contains(out, `"name":""`) {
		t.Fatalf("不应残留空 name 残片:\n%s", out)
	}
}

// TestStitcherFragmentThenCompleteFC 残片A+残片B 合并 + 完整 fc（并行第二调用形态）透传
func TestStitcherFragmentThenCompleteFC(t *testing.T) {
	stream := fragA + fragB + completeFC + terminal
	out := runStitcher(stream, nil)

	// 残片A+残片B 合并成 1 个 + 完整fc 1 个 + 终止 1 个 = 3 个事件
	if got := strings.Count(out, "data:"); got != 3 {
		t.Fatalf("期望 3 个事件，实际 %d:\n%s", got, out)
	}
	if !strings.Contains(out, `"name":"view_file"`) {
		t.Fatalf("完整 fc 应透传:\n%s", out)
	}
}

// TestStitcherFragmentAAloneEOF 残片A 后流直接结束 → 残片A 原样放出（不吞数据）
func TestStitcherFragmentAAloneEOF(t *testing.T) {
	out := runStitcher(fragA, nil)

	if out != fragA {
		t.Fatalf("残片A 应原样（字节不变）放出:\nwant: %q\ngot:  %q", fragA, out)
	}
}

// TestStitcherFragmentAThenText 残片A 后跟文本事件 → 残片A 放出 + 文本透传
func TestStitcherFragmentAThenText(t *testing.T) {
	stream := fragA + buildGeminiTextEvent("你好") + terminal
	out := runStitcher(stream, nil)

	if got := strings.Count(out, "data:"); got != 3 {
		t.Fatalf("残片A+文本+终止 = 3 个事件全放出，实际 %d:\n%s", got, out)
	}
	if !strings.Contains(out, "你好") {
		t.Fatalf("文本事件应透传:\n%s", out)
	}
}

// TestStitcherPassthroughTextAndCompleteFC 纯文本流和完整 fc 流 → 全透传不变
func TestStitcherPassthroughTextAndCompleteFC(t *testing.T) {
	stream := buildGeminiTextEvent("你好") + completeFC + terminal
	out := runStitcher(stream, nil)

	if out != stream {
		t.Fatalf("非残片流应逐字节透传:\nwant: %q\ngot:  %q", stream, out)
	}
}

// TestStitcherUsageNotLost 合并流中 usageMetadata 不丢
func TestStitcherUsageNotLost(t *testing.T) {
	stitcher := &geminiFunctionCallStitcher{}
	requestLog := &ReqeustLog{}

	var out strings.Builder
	for _, ev := range []string{fragA, fragB, terminal} {
		parseGeminiSSELine(ev, requestLog)
		if got := stitcher.process(ev); got != "" {
			out.WriteString(got)
		}
	}

	if requestLog.InputTokens != 12 {
		t.Fatalf("usage 解析应正常工作（promptTokenCount=12），实际 input=%d", requestLog.InputTokens)
	}
	if !strings.Contains(out.String(), "usageMetadata") {
		t.Fatalf("输出流应保留 usageMetadata:\n%s", out.String())
	}
}

// TestStitcherCRLFEvents CRLF 分隔的事件也能识别与合并
func TestStitcherCRLFEvents(t *testing.T) {
	fragACrlf := strings.ReplaceAll(fragA, "\n", "\r\n")
	fragBCrlf := strings.ReplaceAll(fragB, "\n", "\r\n")

	stitcher := &geminiFunctionCallStitcher{}
	var out strings.Builder
	for _, ev := range extractGeminiSSEEvents(fragACrlf+fragBCrlf, &strings.Builder{}) {
		if got := stitcher.process(ev); got != "" {
			out.WriteString(got)
		}
	}
	_ = stitcher.flushPending()

	if !strings.Contains(out.String(), `"name":"run_command"`) || !strings.Contains(out.String(), `"CommandLine":"ls -la"`) {
		t.Fatalf("CRLF 分隔的残片也应合并:\n%s", out.String())
	}
}

// TestClassifyFragment 类别判定单测
func TestClassifyFragment(t *testing.T) {
	if got := classifyGeminiFunctionCallEvent(fragA); got != stitchFragmentA {
		t.Errorf("fragA 应判定为残片A，实际 %d", got)
	}
	if got := classifyGeminiFunctionCallEvent(fragB); got != stitchFragmentB {
		t.Errorf("fragB 应判定为残片B，实际 %d", got)
	}
	if got := classifyGeminiFunctionCallEvent(completeFC); got != stitchNotFragment {
		t.Errorf("完整 fc 应判定为非残片，实际 %d", got)
	}
	if got := classifyGeminiFunctionCallEvent(buildGeminiTextEvent("hi")); got != stitchNotFragment {
		t.Errorf("文本事件应判定为非残片，实际 %d", got)
	}
	if got := classifyGeminiFunctionCallEvent(terminal); got != stitchNotFragment {
		t.Errorf("终止事件应判定为非残片，实际 %d", got)
	}
}

// ---------- E2E：完整 relay 路由 ----------

// newGeminiStitchTestEnv 构建可指定 fixFunctionCallFragments 的测试 relay 环境
func newGeminiStitchTestEnv(t *testing.T, providerID, upstreamURL string, fixFragments bool) (*gin.Engine, *CodexRelayKey) {
	t.Helper()

	_, relayService := newTestRelayService(t)
	if err := relayService.geminiService.AddProvider(GeminiProvider{
		ID:                       providerID,
		Name:                     "GeminiProvider",
		BaseURL:                  upstreamURL,
		APIKey:                   "provider-api-key",
		Model:                    "gemini-2.5-pro",
		Enabled:                  true,
		Level:                    1,
		FixFunctionCallFragments: fixFragments,
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

// geminiFragUpstream 返回模拟 a6api 残片序列的流式上游
func geminiFragUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, fragA)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, fragB)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, terminal)
		w.(http.Flusher).Flush()
	}))
}

// TestGeminiStreamStitchE2E 开启 fixFunctionCallFragments 的 provider，客户端收到完整 functionCall
func TestGeminiStreamStitchE2E(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := geminiFragUpstream(t)
	defer upstream.Close()

	router, relayKey := newGeminiStitchTestEnv(t, "stitch-e2e", upstream.URL, true)

	req := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", relayKey.Key)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d，body: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if strings.Contains(body, `"name":""`) {
		t.Fatalf("缝合后不应有空 name 残片:\n%s", body)
	}
	if !strings.Contains(body, `"name":"run_command"`) {
		t.Fatalf("应包含完整 functionCall name:\n%s", body)
	}
	if !strings.Contains(body, `"CommandLine":"ls -la"`) {
		t.Fatalf("应包含完整 args:\n%s", body)
	}
	if got := strings.Count(body, "data:"); got != 2 {
		t.Fatalf("期望 2 个事件（合并 fc + 终止），实际 %d:\n%s", got, body)
	}
}

// TestGeminiStreamNoStitchByDefault 开关默认关闭：残片原样透传（现有行为不变）
func TestGeminiStreamNoStitchByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := geminiFragUpstream(t)
	defer upstream.Close()

	router, relayKey := newGeminiStitchTestEnv(t, "stitch-off", upstream.URL, false)

	req := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", relayKey.Key)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d，body: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if body != fragA+fragB+terminal {
		t.Fatalf("开关关闭时应逐字节透传:\nwant: %q\ngot:  %q", fragA+fragB+terminal, body)
	}
}
