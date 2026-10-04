package services

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const PelicanPrompt = "创建一个 HTML，内容是 SVG 绘制一个鹈鹕骑自行车的2D 动画"

const (
	pelicanOutputLimit   = 1 << 20
	pelicanResponseLimit = 8 << 20
	pelicanRetention     = 30 * time.Minute
	pelicanMaxRunning    = 4
	pelicanMaxResults    = 20
)

type PelicanTestResult struct {
	SessionID    string `json:"sessionId"`
	ProviderID   int64  `json:"providerId"`
	Model        string `json:"model"`
	ActualModel  string `json:"actualModel"`
	Status       string `json:"status"`
	ErrorCode    string `json:"errorCode,omitempty"`
	Message      string `json:"message,omitempty"`
	RawOutput    string `json:"rawOutput"`
	HTML         string `json:"html"`
	PreviewToken string `json:"previewToken,omitempty"`
	FirstTokenMs *int64 `json:"firstTokenMs"`
	ElapsedMs    int64  `json:"elapsedMs"`
	TotalBytes   int    `json:"totalBytes"`
}

type PelicanProgressEvent struct {
	SessionID string `json:"sessionId"`
	Status    string `json:"status"`
}

type PelicanStreamEvent struct {
	SessionID  string `json:"sessionId"`
	Chunk      string `json:"chunk"`
	StartBytes int    `json:"startBytes"`
	TotalBytes int    `json:"totalBytes"`
}

type pelicanSession struct {
	result       PelicanTestResult
	started      time.Time
	finished     time.Time
	cancel       context.CancelFunc
	pending      strings.Builder
	pendingStart int
	lastEmit     time.Time
}

type PelicanTestService struct {
	providerService *ProviderService
	emitter         EventEmitter
	client          *http.Client
	mu              sync.Mutex
	sessions        map[string]*pelicanSession
	stopped         bool
	workers         sync.WaitGroup
}

func NewPelicanTestService(providers *ProviderService) *PelicanTestService {
	return &PelicanTestService{
		providerService: providers,
		client:          &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		sessions:        make(map[string]*pelicanSession),
	}
}

func (service *PelicanTestService) SetEventEmitter(emitter EventEmitter) {
	service.emitter = emitter
}

func (service *PelicanTestService) Stop() error {
	service.mu.Lock()
	service.stopped = true
	for _, session := range service.sessions {
		session.cancel()
	}
	service.mu.Unlock()
	service.workers.Wait()
	service.client.CloseIdleConnections()
	return nil
}

func pelicanToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func (service *PelicanTestService) StartTest(platform string, providerID int64, model string) (PelicanTestResult, error) {
	model = strings.TrimSpace(model)
	if model == "" || strings.ContainsAny(model, "*?\r\n") || len(model) > 256 {
		return PelicanTestResult{}, errors.New("请输入具体的模型名称，不支持通配符")
	}
	customID := strings.TrimPrefix(platform, "custom:")
	validCustom := strings.HasPrefix(platform, "custom:") && customID != "" && customID != "." && customID != ".." && !strings.ContainsAny(customID, "/\\\x00")
	if platform != "claude" && platform != "codex" && !validCustom {
		return PelicanTestResult{}, errors.New("该平台不支持鹈鹕测试")
	}
	if service.providerService == nil {
		return PelicanTestResult{}, errors.New("供应商服务未配置")
	}
	providers, err := service.providerService.LoadProviders(platform)
	if err != nil {
		return PelicanTestResult{}, errors.New("读取供应商失败")
	}
	var selected *Provider
	for _, provider := range providers {
		if provider.ID == providerID {
			copy := provider
			selected = &copy
			break
		}
	}
	if selected == nil {
		return PelicanTestResult{}, errors.New("未找到指定供应商")
	}
	actual := selected.GetEffectiveModel(model)
	if actual == "" || strings.ContainsAny(actual, "*?\r\n") {
		return PelicanTestResult{}, errors.New("模型映射没有得到具体模型名称")
	}
	id, err := pelicanToken()
	if err != nil {
		return PelicanTestResult{}, errors.New("创建测试会话失败")
	}
	previewToken, err := pelicanToken()
	if err != nil {
		return PelicanTestResult{}, errors.New("创建预览令牌失败")
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.stopped {
		return PelicanTestResult{}, errors.New("测试服务已停止")
	}
	service.pruneLocked()
	running := 0
	for _, session := range service.sessions {
		if session.result.Status == "running" {
			running++
		}
	}
	if running >= pelicanMaxRunning {
		return PelicanTestResult{}, errors.New("已有 4 个测试正在运行，请稍后重试")
	}
	ctx, cancel := context.WithCancel(context.Background())
	session := &pelicanSession{
		result:  PelicanTestResult{SessionID: "pelican-" + id, ProviderID: providerID, Model: model, ActualModel: actual, Status: "running", PreviewToken: previewToken},
		started: time.Now(), cancel: cancel,
	}
	service.sessions[session.result.SessionID] = session
	service.workers.Add(1)
	go service.run(ctx, session, selected, platform)
	return session.result, nil
}

func (service *PelicanTestService) pruneLocked() {
	now := time.Now()
	for id, session := range service.sessions {
		if !session.finished.IsZero() && now.Sub(session.finished) >= pelicanRetention {
			delete(service.sessions, id)
		}
	}
	for {
		completed := 0
		var oldest *pelicanSession
		for _, session := range service.sessions {
			if session.finished.IsZero() {
				continue
			}
			completed++
			if oldest == nil || session.finished.Before(oldest.finished) {
				oldest = session
			}
		}
		if completed <= pelicanMaxResults {
			break
		}
		delete(service.sessions, oldest.result.SessionID)
	}
}

func (service *PelicanTestService) GetTest(sessionID string) (PelicanTestResult, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.pruneLocked()
	session := service.sessions[sessionID]
	if session == nil {
		return PelicanTestResult{}, errors.New("测试会话不存在或已过期")
	}
	result := session.result
	if result.Status == "running" {
		result.ElapsedMs = time.Since(session.started).Milliseconds()
	}
	return result, nil
}

func (service *PelicanTestService) CancelTest(sessionID string) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	session := service.sessions[sessionID]
	if session == nil {
		return errors.New("测试会话不存在或已过期")
	}
	if session.result.Status == "running" {
		session.cancel()
	}
	return nil
}

func (service *PelicanTestService) PreviewHTML(sessionID, token string) (string, bool) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.pruneLocked()
	session := service.sessions[sessionID]
	if session == nil || session.result.Status != "completed" || session.result.HTML == "" ||
		subtle.ConstantTimeCompare([]byte(token), []byte(session.result.PreviewToken)) != 1 {
		return "", false
	}
	return session.result.HTML, true
}

func (service *PelicanTestService) appendText(session *pelicanSession, text string) error {
	if text == "" {
		return nil
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	remaining := pelicanOutputLimit - len(session.result.RawOutput)
	tooLong := len(text) > remaining
	if tooLong {
		text = text[:remaining]
		for !utf8.ValidString(text) && len(text) > 0 {
			text = text[:len(text)-1]
		}
	}
	if text != "" {
		if session.result.FirstTokenMs == nil {
			elapsed := time.Since(session.started).Milliseconds()
			session.result.FirstTokenMs = &elapsed
		}
		if session.pending.Len() == 0 {
			session.pendingStart = len(session.result.RawOutput)
		}
		session.result.RawOutput += text
		session.result.TotalBytes = len(session.result.RawOutput)
		session.pending.WriteString(text)
		if time.Since(session.lastEmit) >= 50*time.Millisecond {
			service.flushLocked(session)
		}
	}
	if tooLong {
		return &pelicanError{code: "output_limit", message: "生成内容超过 1 MiB 限制，已停止测试"}
	}
	return nil
}

func (service *PelicanTestService) flushLocked(session *pelicanSession) {
	if session.pending.Len() == 0 {
		return
	}
	if service.emitter != nil {
		service.emitter.Emit("pelican:stream", PelicanStreamEvent{SessionID: session.result.SessionID, Chunk: session.pending.String(), StartBytes: session.pendingStart, TotalBytes: len(session.result.RawOutput)})
	}
	session.pending.Reset()
	session.lastEmit = time.Now()
}

type pelicanError struct{ code, message string }

func (failure *pelicanError) Error() string { return failure.message }

func (service *PelicanTestService) run(ctx context.Context, session *pelicanSession, provider *Provider, platform string) {
	defer service.workers.Done()
	defer session.cancel()
	err := service.request(ctx, session, provider, platform)
	service.mu.Lock()
	defer service.mu.Unlock()
	service.flushLocked(session)
	session.finished = time.Now()
	session.result.ElapsedMs = session.finished.Sub(session.started).Milliseconds()
	session.result.Status = "completed"
	if err != nil || ctx.Err() != nil {
		session.result.Status = "failed"
		session.result.ErrorCode = "upstream_error"
		session.result.Message = "上游请求失败"
		var failure *pelicanError
		if errors.As(err, &failure) {
			session.result.ErrorCode, session.result.Message = failure.code, failure.message
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			session.result.Status, session.result.ErrorCode, session.result.Message = "cancelled", "cancelled", "测试已取消"
		}
	} else {
		session.result.HTML = extractPelicanHTML(session.result.RawOutput)
		if session.result.HTML == "" {
			session.result.Status, session.result.ErrorCode, session.result.Message = "failed", "no_html", "回答中没有可预览的完整 HTML"
		}
	}
	if service.emitter != nil {
		service.emitter.Emit("pelican:progress", PelicanProgressEvent{SessionID: session.result.SessionID, Status: session.result.Status})
	}
	service.pruneLocked()
}

func (service *PelicanTestService) request(ctx context.Context, session *pelicanSession, provider *Provider, platform string) error {
	endpoint := resolveConnectivityEndpoint(provider, platform)
	anthropic := provider.ResolveUpstreamProtocol(endpoint) == UpstreamProtocolAnthropic
	responses := !anthropic && strings.Contains(strings.ToLower(endpoint), "/responses")
	var body []byte
	var err error
	switch {
	case anthropic:
		body, err = buildAnthropicCompletionBody(session.result.ActualModel, PelicanPrompt, true, 8192)
	case responses:
		body, err = buildResponsesChallengeBody(session.result.ActualModel, PelicanPrompt, true)
	default:
		body, err = buildOpenAIChallengeBody(session.result.ActualModel, PelicanPrompt, true)
	}
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, joinURL(provider.APIURL, endpoint), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if provider.APIKey != "" {
		name, value := completionAuthHeader(provider.ConnectivityAuthType, provider.APIKey)
		req.Header.Set(name, value)
	}
	if anthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	response, err := service.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &pelicanError{code: "http_error", message: fmt.Sprintf("上游返回 HTTP %d", response.StatusCode)}
	}
	limited := &io.LimitedReader{R: response.Body, N: pelicanResponseLimit + 1}
	if strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		err = consumePelicanSSE(limited, func(text string) error { return service.appendText(session, text) })
	} else {
		var data []byte
		data, err = io.ReadAll(limited)
		if err == nil && limited.N > 0 {
			var text, stop string
			text, stop, err = extractPelicanJSON(data, anthropic)
			if err == nil {
				err = service.appendText(session, text)
			}
			if err == nil {
				err = pelicanStopError(stop)
			}
		}
	}
	if limited.N == 0 {
		return &pelicanError{code: "response_limit", message: "上游响应超过读取限制"}
	}
	return err
}

func pelicanStopError(stop string) error {
	switch stop {
	case "stop", "end_turn", "stop_sequence", "completed":
		return nil
	case "refusal", "content_filter":
		return &pelicanError{code: "refused", message: "模型拒绝生成或内容被过滤"}
	case "length", "max_tokens", "max_output_tokens", "incomplete":
		return &pelicanError{code: "truncated", message: "回答被截断，未自动加载预览"}
	default:
		return &pelicanError{code: "incomplete", message: "上游未正常结束回答，未自动加载预览"}
	}
}

func consumePelicanSSE(reader io.Reader, appendText func(string) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), pelicanResponseLimit)
	var data []string
	stop := ""
	done := false
	process := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		if payload == "[DONE]" {
			done = true
			return nil
		}
		var event struct {
			Type    string          `json:"type"`
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Refusal string `json:"refusal"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &event) != nil {
			return &pelicanError{code: "invalid_response", message: "上游流式响应格式无效"}
		}
		if event.Type == "error" || event.Type == "response.failed" || (len(event.Error) > 0 && string(event.Error) != "null") {
			return &pelicanError{code: "upstream_error", message: "上游报告生成失败"}
		}
		text, reason, err := extractCompletionDelta(payload, false)
		if err != nil {
			return err
		}
		if err = appendText(text); err != nil {
			return err
		}
		if len(event.Choices) > 0 {
			if event.Choices[0].Delta.Refusal != "" {
				reason = "refusal"
			}
			if event.Choices[0].FinishReason != "" {
				reason = event.Choices[0].FinishReason
			}
		}
		if event.Type == "response.refusal.delta" {
			reason = "refusal"
		}
		if event.Type == "response.incomplete" && reason == "" {
			reason = "incomplete"
		}
		if reason != "" {
			stop = reason
			done = true
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := process(); err != nil {
				return err
			}
			if done {
				return pelicanStopError(stop)
			}
		} else if after, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(after, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return &pelicanError{code: "response_limit", message: "上游流式事件超过读取限制"}
		}
		return err
	}
	if err := process(); err != nil {
		return err
	}
	return pelicanStopError(stop)
}

func extractPelicanJSON(data []byte, anthropic bool) (string, string, error) {
	var payload struct {
		Content    []struct{ Type, Text string } `json:"content"`
		StopReason string                        `json:"stop_reason"`
		Choices    []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
				Refusal string          `json:"refusal"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []struct {
			Content []struct{ Type, Text string } `json:"content"`
		} `json:"output"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return "", "", &pelicanError{code: "invalid_response", message: "上游响应不是有效 JSON"}
	}
	if anthropic {
		var text strings.Builder
		for _, block := range payload.Content {
			if block.Type == "text" {
				text.WriteString(block.Text)
			}
		}
		return text.String(), payload.StopReason, nil
	}
	if len(payload.Choices) > 0 {
		choice := payload.Choices[0]
		if choice.Message.Refusal != "" {
			return rawContentText(choice.Message.Content), "refusal", nil
		}
		return rawContentText(choice.Message.Content), choice.FinishReason, nil
	}
	var text strings.Builder
	for _, output := range payload.Output {
		for _, part := range output.Content {
			if part.Type == "refusal" {
				return text.String(), "refusal", nil
			}
			if part.Type == "output_text" || part.Type == "text" {
				text.WriteString(part.Text)
			}
		}
	}
	if payload.IncompleteDetails != nil {
		return text.String(), "incomplete", nil
	}
	return text.String(), payload.Status, nil
}

var pelicanDocumentPattern = regexp.MustCompile(`(?is)(?:<!doctype\s+html\s*>\s*)?<html\b[^>]*>.*?</html\s*>`)
var pelicanFencePattern = regexp.MustCompile("(?is)```html[ \\t]*\\r?\\n(.*?)\\r?\\n```")

func extractPelicanHTML(raw string) string {
	if document := pelicanDocumentPattern.FindString(raw); document != "" {
		return document
	}
	for _, match := range pelicanFencePattern.FindAllStringSubmatch(raw, -1) {
		candidate := strings.TrimSpace(match[1])
		lower := strings.ToLower(candidate)
		if strings.Contains(lower, "<svg") && strings.Contains(lower, "</svg>") {
			return candidate
		}
	}
	return ""
}
