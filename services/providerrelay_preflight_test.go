package services

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daodao97/xgo/xrequest"
	"github.com/gin-gonic/gin"
)

func useCodexPreflightTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	previous := codexResponsePreflightTimeout
	codexResponsePreflightTimeout = timeout
	t.Cleanup(func() {
		codexResponsePreflightTimeout = previous
	})
}

func newCodexPreflightTestResponse(status int, contentType string, body io.ReadCloser) *xrequest.Response {
	return xrequest.NewResponse(&http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       body,
	})
}

func TestPostCodexResponsesRequestBoundsSlowForbiddenBody(t *testing.T) {
	useCodexPreflightTimeout(t, 40*time.Millisecond)

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"quota exhausted"}`))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		upstream.Close()
	}()

	relay := &ProviderRelayService{httpClient: newRelayHTTPClient()}
	startedAt := time.Now()
	resp, err := relay.postCodexResponsesRequest(
		context.Background(),
		upstream.URL,
		nil,
		map[string]string{"Content-Type": "application/json"},
		[]byte(`{"model":"gpt-5","stream":true}`),
		"slow-forbidden",
	)
	elapsed := time.Since(startedAt)
	if err != nil {
		t.Fatalf("postCodexResponsesRequest() error = %v", err)
	}
	if resp == nil || resp.StatusCode() != http.StatusForbidden {
		t.Fatalf("status = %v, want 403", resp)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("slow 403 body blocked for %s", elapsed)
	}
	body, readErr := io.ReadAll(resp.RawResponse.Body)
	if readErr != nil {
		t.Fatalf("read buffered body: %v", readErr)
	}
	if got := string(body); got != `{"error":"quota exhausted"}` {
		t.Fatalf("buffered body = %q", got)
	}
}

func TestBufferCodexErrorResponseTruncatesAtSizeLimit(t *testing.T) {
	body := bytes.Repeat([]byte("x"), codexErrorBodyMaxBytes+1024)
	resp := newCodexPreflightTestResponse(
		http.StatusForbidden,
		"application/json",
		io.NopCloser(bytes.NewReader(body)),
	)
	if err := bufferCodexErrorResponse(context.Background(), resp, "large-error-provider"); err != nil {
		t.Fatalf("bufferCodexErrorResponse() error = %v", err)
	}
	buffered, err := io.ReadAll(resp.RawResponse.Body)
	if err != nil {
		t.Fatalf("read buffered body: %v", err)
	}
	if len(buffered) != codexErrorBodyMaxBytes {
		t.Fatalf("buffered size=%d, want %d", len(buffered), codexErrorBodyMaxBytes)
	}
}

func TestCodexHistoryPreflightTimeoutPassesThroughByteExact(t *testing.T) {
	useCodexPreflightTimeout(t, 40*time.Millisecond)

	reader, writer := io.Pipe()
	resp := newCodexPreflightTestResponse(http.StatusOK, "text/event-stream", reader)
	prefix := strings.Join([]string{
		": keepalive",
		"event: response.created",
		`data: {"type":"response.created","response":{"status":"in_progress","output":[]}}`,
		"",
		"",
	}, "\n")
	tail := strings.Join([]string{
		"event: response.failed",
		`data: {"type":"response.failed","response":{"status":"failed","error":{"code":"unknown_parameter","param":"input[24].namespace"}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	type result struct {
		retry bool
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		retry, _, err := codexResponseNeedsProviderHistoryFallback(context.Background(), resp, true, "timeout-provider")
		resultCh <- result{retry: retry, err: err}
	}()
	if _, err := io.WriteString(writer, prefix); err != nil {
		t.Fatalf("write prefix: %v", err)
	}

	select {
	case got := <-resultCh:
		if got.err != nil || got.retry {
			t.Fatalf("preflight result retry=%v err=%v", got.retry, got.err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("preflight did not fail open")
	}
	if !codexResponsePreflightFailedOpen(resp) {
		t.Fatal("timed-out response is not marked fail-open")
	}

	startedAt := time.Now()
	retryNamespace, err := codexResponseNeedsInputNamespaceFallback(context.Background(), resp, true, "timeout-provider")
	if err != nil || retryNamespace {
		t.Fatalf("second preflight retry=%v err=%v", retryNamespace, err)
	}
	if elapsed := time.Since(startedAt); elapsed > 20*time.Millisecond {
		t.Fatalf("second preflight stacked another delay: %s", elapsed)
	}

	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := io.WriteString(writer, tail)
		if writeErr == nil {
			writeErr = writer.Close()
		}
		writeDone <- writeErr
	}()
	body, readErr := io.ReadAll(resp.RawResponse.Body)
	if readErr != nil {
		t.Fatalf("read replay body: %v", readErr)
	}
	if writeErr := <-writeDone; writeErr != nil {
		t.Fatalf("write tail: %v", writeErr)
	}
	if got, want := string(body), prefix+tail; got != want {
		t.Fatalf("replayed body changed:\n got: %q\nwant: %q", got, want)
	}
}

func TestCodexHistoryPreflightSizeLimitPassesThroughByteExact(t *testing.T) {
	useCodexPreflightTimeout(t, time.Second)

	reader, writer := io.Pipe()
	resp := newCodexPreflightTestResponse(http.StatusOK, "text/event-stream", reader)
	body := append(bytes.Repeat([]byte(":"), codexHistoryPreflightMaxBytes+123), '\n')

	resultCh := make(chan error, 1)
	go func() {
		retry, _, err := codexResponseNeedsProviderHistoryFallback(context.Background(), resp, true, "large-provider")
		if retry && err == nil {
			err = errors.New("unexpected retry")
		}
		resultCh <- err
	}()
	writeDone := make(chan error, 1)
	go func() {
		_, err := writer.Write(body)
		if err == nil {
			err = writer.Close()
		}
		writeDone <- err
	}()

	select {
	case err := <-resultCh:
		if err != nil {
			t.Fatalf("preflight error: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("size-limited preflight did not fail open")
	}
	replayed, err := io.ReadAll(resp.RawResponse.Body)
	if err != nil {
		t.Fatalf("read replayed body: %v", err)
	}
	if writeErr := <-writeDone; writeErr != nil {
		t.Fatalf("write body: %v", writeErr)
	}
	if !bytes.Equal(replayed, body) {
		t.Fatalf("size fail-open changed body: got=%d want=%d", len(replayed), len(body))
	}
}

func TestCodexHistoryPreflightStopsOnClientCancel(t *testing.T) {
	useCodexPreflightTimeout(t, time.Second)

	reader, writer := io.Pipe()
	resp := newCodexPreflightTestResponse(http.StatusOK, "text/event-stream", reader)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan error, 1)
	go func() {
		_, _, err := codexResponseNeedsProviderHistoryFallback(ctx, resp, true, "canceled-provider")
		resultCh <- err
	}()
	if _, err := io.WriteString(writer, ": keepalive\n\n"); err != nil {
		t.Fatalf("write keepalive: %v", err)
	}
	cancel()

	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("preflight error = %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("canceled preflight did not return")
	}
	_ = writer.Close()
}

func TestCodexHistoryPreflightEmptyStreamStillRetries(t *testing.T) {
	resp := newCodexPreflightTestResponse(
		http.StatusOK,
		"text/event-stream",
		io.NopCloser(strings.NewReader("")),
	)
	retry, reason, err := codexResponseNeedsProviderHistoryFallback(context.Background(), resp, true, "empty-provider")
	if err != nil {
		t.Fatalf("preflight error: %v", err)
	}
	if !retry || reason != "stream_without_output" {
		t.Fatalf("retry=%v reason=%q", retry, reason)
	}
}

func TestCodexNamespacePreflightInspectsFinalLineWithoutNewline(t *testing.T) {
	body := `data: {"type":"response.failed","error":{"code":"unknown_parameter","param":"input[24].namespace"}}`
	resp := newCodexPreflightTestResponse(
		http.StatusOK,
		"text/event-stream",
		io.NopCloser(strings.NewReader(body)),
	)
	retry, err := codexResponseNeedsInputNamespaceFallback(context.Background(), resp, true, "final-line-provider")
	if err != nil {
		t.Fatalf("preflight error: %v", err)
	}
	if !retry {
		t.Fatal("final SSE line was not inspected")
	}
}

func TestCodexPreflightTimeoutFlushesHeadersBeforeUpstreamBody(t *testing.T) {
	useCodexPreflightTimeout(t, 40*time.Millisecond)
	gin.SetMode(gin.TestMode)

	upstreamHeaders := make(chan struct{})
	releaseBody := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseBody) }) }
	defer release()
	upstreamBody := codexSessionSSE(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"released"}]}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(upstreamHeaders)
		select {
		case <-releaseBody:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	providers, relay := newTestRelayService(t)
	setNamespaceTestBlacklistEnabled(t, false)
	if err := providers.SaveProviders(ProviderKindCodex, []Provider{{
		ID: 1, Name: "header-flush-provider", APIURL: upstream.URL, APIKey: "key", Enabled: true,
		CodexMultiAgentNamespaceRewrite: true,
	}}); err != nil {
		t.Fatalf("SaveProviders: %v", err)
	}
	relayKey, err := relay.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatalf("EnsureDefaultKey: %v", err)
	}
	router := gin.New()
	relay.registerRoutes(router)
	relayServer := httptest.NewServer(router)
	defer relayServer.Close()

	req, err := http.NewRequest(
		http.MethodPost,
		relayServer.URL+"/responses",
		bytes.NewReader(codexInputNamespaceRequest("header-flush-session", true)),
	)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+relayKey.Key)

	type responseResult struct {
		resp *http.Response
		err  error
	}
	responseCh := make(chan responseResult, 1)
	startedAt := time.Now()
	go func() {
		resp, doErr := http.DefaultClient.Do(req)
		responseCh <- responseResult{resp: resp, err: doErr}
	}()
	select {
	case <-upstreamHeaders:
	case <-time.After(time.Second):
		t.Fatal("upstream headers were not sent")
	}

	var clientResp *http.Response
	select {
	case result := <-responseCh:
		if result.err != nil {
			t.Fatalf("relay request failed: %v", result.err)
		}
		clientResp = result.resp
	case <-time.After(500 * time.Millisecond):
		t.Fatal("relay did not flush headers after preflight timeout")
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("relay headers took %s", elapsed)
	}
	if clientResp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", clientResp.StatusCode)
	}

	release()
	body, readErr := io.ReadAll(clientResp.Body)
	_ = clientResp.Body.Close()
	if readErr != nil {
		t.Fatalf("read client body: %v", readErr)
	}
	if string(body) != upstreamBody {
		t.Fatalf("body changed:\n got: %q\nwant: %q", body, upstreamBody)
	}
}

func TestAsyncImagePollingDoesNotBlockCodexHeaders(t *testing.T) {
	previousInterval := asyncImagePollInterval
	asyncImagePollInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		asyncImagePollInterval = previousInterval
	})

	var taskReleased atomic.Bool
	pollStarted := make(chan struct{})
	var signalPoll sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/images/generations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"task-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/task-1":
			signalPoll.Do(func() { close(pollStarted) })
			w.Header().Set("Content-Type", "application/json")
			if taskReleased.Load() {
				_, _ = w.Write([]byte(`{"state":"succeeded","data":{"images":[{"url":"https://example.test/image.png"}]}}`))
				return
			}
			_, _ = w.Write([]byte(`{"state":"pending"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.created\"}\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	relay := &ProviderRelayService{httpClient: newRelayHTTPClient()}
	provider := Provider{Name: "shared-upstream", APIURL: upstream.URL, APIKey: "key"}
	gin.SetMode(gin.TestMode)
	imageRecorder := httptest.NewRecorder()
	imageContext, _ := gin.CreateTestContext(imageRecorder)
	imageContext.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2"}`))

	imageDone := make(chan struct {
		ok  bool
		err error
	}, 1)
	go func() {
		ok, err := relay.forwardOpenAIImageRequest(
			imageContext,
			ProviderKindGPTImage,
			provider,
			"/v1/images/generations",
			nil,
			map[string]string{"Content-Type": "application/json"},
			[]byte(`{"model":"gpt-image-2"}`),
			"gpt-image-2",
			false,
			true,
		)
		imageDone <- struct {
			ok  bool
			err error
		}{ok: ok, err: err}
	}()

	select {
	case <-pollStarted:
	case <-time.After(time.Second):
		t.Fatal("image task did not enter polling")
	}

	startedAt := time.Now()
	codexResp, err := relay.postCodexResponsesRequest(
		context.Background(),
		upstream.URL+"/responses",
		nil,
		map[string]string{"Content-Type": "application/json"},
		[]byte(`{"model":"gpt-5","stream":true}`),
		"shared-upstream",
	)
	if err != nil {
		t.Fatalf("Codex request failed during image polling: %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 200*time.Millisecond {
		t.Fatalf("Codex headers waited for image polling: %s", elapsed)
	}
	_ = codexResp.RawResponse.Body.Close()

	taskReleased.Store(true)
	select {
	case result := <-imageDone:
		if result.err != nil || !result.ok {
			t.Fatalf("image request ok=%v err=%v", result.ok, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("image request did not finish after task release")
	}
}
