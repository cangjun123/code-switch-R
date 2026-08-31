package services

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/daodao97/xgo/xrequest"
)

// codexQuotaAttemptTracker is carried through the request context.  The
// lowest-level upstream request attaches a tracking body, so every actual HTTP
// attempt (including compatibility retries hidden inside the Responses path)
// gets one independent settlement.
type codexQuotaAttemptTracker struct {
	service  *ProviderRelayService
	keyID    string
	provider string
	model    string
}

type codexQuotaAttemptContextKey struct{}

// A relay response can be an unbounded SSE stream.  Keep a bounded tail for
// non-streaming JSON/final-event parsing, while parsing complete SSE records as
// they arrive so an early usage event is not lost when a provider sends a long
// tail of output afterwards.
const (
	codexQuotaTrackingMaxBufferBytes = 1 << 20 // 1 MiB
	codexQuotaTrackingMaxLineBytes   = 256 << 10
	relayQuotaSettlementLogInterval  = 30 * time.Second
)

var relayQuotaSettlementLogState struct {
	sync.Mutex
	last       time.Time
	suppressed int
}

func withCodexQuotaAttemptTracker(ctx context.Context, tracker *codexQuotaAttemptTracker) context.Context {
	if ctx == nil || tracker == nil {
		return ctx
	}
	return context.WithValue(ctx, codexQuotaAttemptContextKey{}, tracker)
}

func codexQuotaTrackerFromContext(ctx context.Context) *codexQuotaAttemptTracker {
	if ctx == nil {
		return nil
	}
	tracker, _ := ctx.Value(codexQuotaAttemptContextKey{}).(*codexQuotaAttemptTracker)
	return tracker
}

func (t *codexQuotaAttemptTracker) settle(attemptID string, body []byte) {
	var logEntry ReqeustLog
	parseCodexQuotaUsage(body, &logEntry)
	t.settleLog(attemptID, logEntry)
}

func parseCodexQuotaUsage(body []byte, logEntry *ReqeustLog) {
	if len(body) == 0 || logEntry == nil {
		return
	}
	payload := string(body)
	// SSE responses contain one or more `data:` records; parsing the whole
	// stream as JSON would silently lose the final usage event.
	parseEventPayload(payload, CodexParseTokenUsageFromResponse, logEntry)
	CodexParseTokenUsageFromResponse(payload, logEntry)
	// If a very large non-streaming JSON response was tail-buffered, the tail
	// may not itself be a complete JSON document.  Recover a complete usage
	// object from the fragment without retaining the full generated output.
	parseCodexUsageObjectFragments(body, logEntry)
}

func parseCodexUsageObjectFragments(body []byte, logEntry *ReqeustLog) {
	marker := []byte(`"usage"`)
	for searchEnd := len(body); searchEnd > 0; {
		index := bytes.LastIndex(body[:searchEnd], marker)
		if index < 0 {
			return
		}
		searchEnd = index
		position := index + len(marker)
		for position < len(body) && (body[position] == ' ' || body[position] == '\t' || body[position] == '\r' || body[position] == '\n') {
			position++
		}
		if position >= len(body) || body[position] != ':' {
			continue
		}
		position++
		for position < len(body) && (body[position] == ' ' || body[position] == '\t' || body[position] == '\r' || body[position] == '\n') {
			position++
		}
		object, ok := completeJSONObjectAt(body, position)
		if !ok {
			continue
		}
		synthetic := make([]byte, 0, len(object)+12)
		synthetic = append(synthetic, `{"usage":`...)
		synthetic = append(synthetic, object...)
		synthetic = append(synthetic, '}')
		CodexParseTokenUsageFromResponse(string(synthetic), logEntry)
	}
}

func completeJSONObjectAt(data []byte, start int) ([]byte, bool) {
	if start < 0 || start >= len(data) || data[start] != '{' {
		return nil, false
	}
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(data); index++ {
		value := data[index]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if value == '\\' {
				escaped = true
				continue
			}
			if value == '"' {
				inString = false
			}
			continue
		}
		switch value {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return data[start : index+1], true
			}
			if depth < 0 {
				return nil, false
			}
		}
	}
	return nil, false
}

func (t *codexQuotaAttemptTracker) settleLog(attemptID string, logEntry ReqeustLog) {
	if t == nil || t.service == nil || t.service.relayQuota == nil || t.service.codexRelayKeys == nil {
		return
	}
	keyID := t.keyID
	if keyID == "" {
		return
	}
	key, err := t.service.codexRelayKeys.GetKeyByID(keyID)
	if err != nil {
		logRelayQuotaSettlementError(attemptID, t.provider, t.model, err)
		return
	}
	usage := RelayQuotaUsage{
		InputTokens:           int64(logEntry.InputTokens),
		CachedInputTokens:     int64(logEntry.CacheReadTokens),
		OutputTokens:          int64(logEntry.OutputTokens),
		ReasoningOutputTokens: int64(logEntry.ReasoningTokens),
	}
	if _, err := t.service.relayQuota.Settle(key, attemptID, t.provider, t.model, usage); err != nil {
		// Settlement happens after the response body is consumed and must not
		// change the already-delivered upstream response.  Make failures visible
		// to operators instead of silently dropping accounting errors.
		logRelayQuotaSettlementError(attemptID, t.provider, t.model, err)
	}
}

func logRelayQuotaSettlementError(attemptID, provider, model string, err error) {
	if err == nil {
		return
	}
	now := time.Now()
	relayQuotaSettlementLogState.Lock()
	if !relayQuotaSettlementLogState.last.IsZero() && now.Sub(relayQuotaSettlementLogState.last) < relayQuotaSettlementLogInterval {
		relayQuotaSettlementLogState.suppressed++
		relayQuotaSettlementLogState.Unlock()
		return
	}
	suppressed := relayQuotaSettlementLogState.suppressed
	relayQuotaSettlementLogState.last = now
	relayQuotaSettlementLogState.suppressed = 0
	relayQuotaSettlementLogState.Unlock()

	sanitize := func(value string) string {
		value = strings.Join(strings.Fields(value), " ")
		if len(value) > 160 {
			value = value[:160] + "..."
		}
		return value
	}
	fmt.Printf(
		"[Relay Quota] level=error event=settlement_failed attempt=%s provider=%q model=%q suppressed=%d error=%q\n",
		sanitize(attemptID), sanitize(provider), sanitize(model), suppressed, sanitize(err.Error()),
	)
}

// codexQuotaTrackingBody buffers only response bytes needed for usage parsing;
// it does not alter what is returned to the caller.  Finish is idempotent so a
// Read(io.EOF) followed by Close cannot double-settle an attempt.
type codexQuotaTrackingBody struct {
	io.ReadCloser
	tracker   *codexQuotaAttemptTracker
	attemptID string
	mu        sync.Mutex
	buffer    []byte
	line      []byte
	usage     ReqeustLog
	once      sync.Once
}

func (b *codexQuotaTrackingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.mu.Lock()
		b.capture(p[:n])
		b.mu.Unlock()
	}
	if err != nil {
		b.finish()
	}
	return n, err
}

func (b *codexQuotaTrackingBody) capture(data []byte) {
	if b == nil || len(data) == 0 {
		return
	}
	// Retain only a bounded tail.  This is enough for ordinary JSON and the
	// final Responses usage event; SSE records are parsed incrementally below.
	if len(data) >= codexQuotaTrackingMaxBufferBytes {
		b.buffer = append(b.buffer[:0], data[len(data)-codexQuotaTrackingMaxBufferBytes:]...)
	} else {
		b.buffer = append(b.buffer, data...)
		if len(b.buffer) > codexQuotaTrackingMaxBufferBytes {
			over := len(b.buffer) - codexQuotaTrackingMaxBufferBytes
			copy(b.buffer, b.buffer[over:])
			b.buffer = b.buffer[:codexQuotaTrackingMaxBufferBytes]
		}
	}

	// Parse complete SSE data lines as they arrive.  A cap prevents a malformed
	// provider line from becoming an unbounded allocation.
	b.line = append(b.line, data...)
	for {
		index := bytes.IndexByte(b.line, '\n')
		if index < 0 {
			break
		}
		line := b.line[:index+1]
		trimmed := strings.TrimSpace(string(line))
		if strings.HasPrefix(trimmed, "data:") {
			CodexParseTokenUsageFromResponse(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")), &b.usage)
		}
		b.line = append([]byte(nil), b.line[index+1:]...)
	}
	if len(b.line) > codexQuotaTrackingMaxLineBytes {
		b.line = append([]byte(nil), b.line[len(b.line)-codexQuotaTrackingMaxLineBytes:]...)
	}
}

func (b *codexQuotaTrackingBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish()
	return err
}

func (b *codexQuotaTrackingBody) finish() {
	if b == nil {
		return
	}
	b.once.Do(func() {
		b.mu.Lock()
		body := append([]byte(nil), b.buffer...)
		logEntry := b.usage
		if len(b.line) > 0 {
			trimmed := strings.TrimSpace(string(b.line))
			if strings.HasPrefix(trimmed, "data:") {
				CodexParseTokenUsageFromResponse(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")), &logEntry)
			}
		}
		b.mu.Unlock()
		// Parse the bounded tail as well.  Max-value parsing makes this safe when
		// the same cumulative usage appears in both the incremental and tail data.
		parseCodexQuotaUsage(body, &logEntry)
		b.tracker.settleLog(b.attemptID, logEntry)
	})
}

func (t *codexQuotaAttemptTracker) attachResponse(resp *xrequest.Response, attemptID string) {
	if t == nil || resp == nil || resp.RawResponse == nil {
		if t != nil {
			t.settle(attemptID, nil)
		}
		return
	}
	if resp.RawResponse.Body == nil {
		t.settle(attemptID, nil)
		return
	}
	resp.RawResponse.Body = &codexQuotaTrackingBody{
		ReadCloser: resp.RawResponse.Body,
		tracker:    t,
		attemptID:  attemptID,
	}
}
