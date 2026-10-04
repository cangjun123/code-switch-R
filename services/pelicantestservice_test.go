package services

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const pelicanTestHTML = `<!DOCTYPE html><html><body><svg><circle cx="10" cy="10" r="5"/></svg><script>document.body.dataset.ready='yes'</script></body></html>`

type pelicanTestTransport func(*http.Request) (*http.Response, error)

func (transport pelicanTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func newPelicanTestService(t *testing.T, platform, endpoint, auth string, handler http.HandlerFunc) *PelicanTestService {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	providers := NewProviderService()
	err := providers.SaveProviders(platform, []Provider{{ID: 42, Name: "pelican", APIURL: upstream.URL, APIKey: "pelican-secret", APIEndpoint: endpoint, UpstreamProtocol: "auto", ConnectivityAuthType: auth, ModelMapping: map[string]string{"alias": "actual-model"}}})
	if err != nil {
		t.Fatal(err)
	}
	service := NewPelicanTestService(providers)
	t.Cleanup(func() { service.Stop() })
	return service
}

func awaitPelicanTest(t *testing.T, service *PelicanTestService, id string) PelicanTestResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		result, err := service.GetTest(id)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "running" {
			return result
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("test did not finish")
	return PelicanTestResult{}
}

func pelicanSSE(payload any) string {
	data, _ := json.Marshal(payload)
	return "data: " + string(data) + "\n\n"
}

func TestPelicanProtocolsAndExactPrompt(t *testing.T) {
	tests := []struct {
		name, platform, endpoint, auth, response string
		stream                                   bool
	}{
		{"anthropic-stream", "claude", "/v1/messages", "x-api-key", pelicanSSE(map[string]any{"type": "content_block_delta", "delta": map[string]string{"type": "text_delta", "text": pelicanTestHTML}}) + pelicanSSE(map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "end_turn"}}), true},
		{"chat-stream", "codex", "/v1/chat/completions", "bearer", pelicanSSE(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": pelicanTestHTML}, "finish_reason": "stop"}}}), true},
		{"responses-stream", "codex", "/responses", "X-Token", pelicanSSE(map[string]any{"type": "response.output_text.delta", "delta": pelicanTestHTML}) + pelicanSSE(map[string]any{"type": "response.completed", "response": map[string]string{"status": "completed"}}), true},
		{"anthropic-json", "claude", "/v1/messages", "x-api-key", `{"content":[{"type":"text","text":` + fmt.Sprintf("%q", pelicanTestHTML) + `}],"stop_reason":"end_turn"}`, false},
		{"chat-json", "custom:test-tool", "/v1/chat/completions", "bearer", `{"choices":[{"message":{"content":` + fmt.Sprintf("%q", pelicanTestHTML) + `},"finish_reason":"stop"}]}`, false},
		{"responses-json", "codex", "/responses", "bearer", `{"status":"completed","output":[{"content":[{"type":"output_text","text":` + fmt.Sprintf("%q", pelicanTestHTML) + `}]}]}`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			service := newPelicanTestService(t, test.platform, test.endpoint, test.auth, func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				expectedPath := strings.TrimPrefix(joinURL("https://example.test", test.endpoint), "https://example.test")
				if request.URL.Path != expectedPath {
					t.Errorf("endpoint = %s", request.URL.Path)
				}
				name, value := completionAuthHeader(test.auth, "pelican-secret")
				if request.Header.Get(name) != value {
					t.Errorf("auth header = %q", request.Header.Get(name))
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if string(body["model"]) != `"actual-model"` || string(body["stream"]) != "true" {
					t.Errorf("request = %s", body)
				}
				for _, forbidden := range []string{"system", "instructions", "tools"} {
					if _, ok := body[forbidden]; ok {
						t.Errorf("unexpected %s", forbidden)
					}
				}
				if strings.Contains(test.endpoint, "responses") {
					var input []struct {
						Role    string
						Content []struct{ Type, Text string }
					}
					if err := json.Unmarshal(body["input"], &input); err != nil {
						t.Error(err)
						return
					}
					if len(input) != 1 || input[0].Role != "user" || len(input[0].Content) != 1 || input[0].Content[0].Text != PelicanPrompt {
						t.Errorf("input = %+v", input)
					}
				} else {
					var messages []struct{ Role, Content string }
					if err := json.Unmarshal(body["messages"], &messages); err != nil {
						t.Error(err)
						return
					}
					if len(messages) != 1 || messages[0].Role != "user" || messages[0].Content != "创建一个 HTML，内容是 SVG 绘制一个鹈鹕骑自行车的2D 动画" {
						t.Errorf("messages = %+v", messages)
					}
				}
				if test.platform == "claude" && (string(body["max_tokens"]) != "8192" || request.Header.Get("anthropic-version") != "2023-06-01") {
					t.Error("missing Anthropic settings")
				}
				if test.stream {
					writer.Header().Set("Content-Type", "text/event-stream")
				} else {
					writer.Header().Set("Content-Type", "application/json")
				}
				io.WriteString(writer, test.response)
			})
			hub := NewEventHub()
			events, unsubscribe := hub.Subscribe(32)
			defer unsubscribe()
			service.SetEventEmitter(hub)
			started, err := service.StartTest(test.platform, 42, "alias")
			if err != nil {
				t.Fatal(err)
			}
			result := awaitPelicanTest(t, service, started.SessionID)
			if result.Status != "completed" || result.HTML != pelicanTestHTML || result.RawOutput != pelicanTestHTML || result.ActualModel != "actual-model" || result.FirstTokenMs == nil {
				t.Fatalf("result = %+v", result)
			}
			if calls.Load() != 1 {
				t.Fatalf("calls = %d", calls.Load())
			}
			if html, ok := service.PreviewHTML(started.SessionID, result.PreviewToken); !ok || html != pelicanTestHTML {
				t.Fatal("preview not available")
			}
			if _, ok := service.PreviewHTML(started.SessionID, "wrong"); ok {
				t.Fatal("wrong token accepted")
			}
			foundStream, foundProgress := false, false
			for len(events) > 0 {
				event := <-events
				if event.Name == "pelican:stream" {
					payload := event.Data.(PelicanStreamEvent)
					foundStream = payload.SessionID == started.SessionID && payload.TotalBytes == len(pelicanTestHTML)
				}
				if event.Name == "pelican:progress" {
					payload := event.Data.(PelicanProgressEvent)
					foundProgress = payload.SessionID == started.SessionID && payload.Status == "completed"
				}
			}
			if !foundStream || !foundProgress {
				t.Fatal("missing session events")
			}
		})
	}
}

func TestPelicanHTMLExtraction(t *testing.T) {
	for _, test := range []struct{ name, raw, expected string }{
		{"document", pelicanTestHTML, pelicanTestHTML},
		{"explanation", "Here is HTML:\n" + pelicanTestHTML + "\nDone.", pelicanTestHTML},
		{"fence", "```html\n" + pelicanTestHTML + "\n```", pelicanTestHTML},
		{"fragment-fence", "```html\n<div><svg></svg></div>\n```", "<div><svg></svg></div>"},
		{"no-html", "I cannot draw this", ""},
		{"truncated", "<html><body><svg>", ""},
		{"bare-svg", "<svg></svg>", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if actual := extractPelicanHTML(test.raw); actual != test.expected {
				t.Fatalf("HTML = %q", actual)
			}
		})
	}
}

func TestPelicanFailuresPreservePartialOutput(t *testing.T) {
	partial := pelicanSSE(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": pelicanTestHTML}}}})
	for _, test := range []struct {
		name, response, code string
		status               int
	}{
		{"truncated", partial + pelicanSSE(map[string]any{"choices": []any{map[string]any{"finish_reason": "length"}}}), "truncated", 200},
		{"refused", partial + pelicanSSE(map[string]any{"type": "response.refusal.delta", "delta": "no"}), "refused", 200},
		{"eof", partial, "incomplete", 200},
		{"done-without-stop", partial + "data: [DONE]\n\n", "incomplete", 200},
		{"upstream-error", partial + pelicanSSE(map[string]any{"type": "error", "error": map[string]string{"message": "pelican-secret"}}), "upstream_error", 200},
		{"invalid-json", "data: not-json\n\n", "invalid_response", 200},
		{"http", "pelican-secret", "http_error", 403},
		{"no-html", pelicanSSE(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": "no HTML"}, "finish_reason": "stop"}}}), "no_html", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			service := newPelicanTestService(t, "codex", "/chat/completions", "bearer", func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				writer.Header().Set("Content-Type", "text/event-stream")
				writer.WriteHeader(test.status)
				io.WriteString(writer, test.response)
			})
			started, err := service.StartTest("codex", 42, "alias")
			if err != nil {
				t.Fatal(err)
			}
			result := awaitPelicanTest(t, service, started.SessionID)
			if result.Status != "failed" || result.ErrorCode != test.code || result.HTML != "" {
				t.Fatalf("result = %+v", result)
			}
			if strings.HasPrefix(test.response, partial) && result.RawOutput != pelicanTestHTML {
				t.Fatal("partial output lost")
			}
			if strings.Contains(result.Message, "pelican-secret") || calls.Load() != 1 {
				t.Fatal("leaked credentials or retried")
			}
			if _, ok := service.PreviewHTML(started.SessionID, result.PreviewToken); ok {
				t.Fatal("failed preview available")
			}
		})
	}
}

func TestPelicanReasoningExcludedAndMultilineSSE(t *testing.T) {
	input := pelicanSSE(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"reasoning_content": "DO NOT RENDER"}}}}) +
		pelicanSSE(map[string]any{"type": "content_block_delta", "delta": map[string]string{"thinking": "DO NOT RENDER"}}) +
		pelicanSSE(map[string]any{"type": "response.reasoning_summary_text.delta", "delta": "DO NOT RENDER"}) +
		"data: {\n" + "data: \"choices\":[{\"delta\":{\"content\":\"HTML\"},\"finish_reason\":\"stop\"}]}\n\n"
	var text strings.Builder
	err := consumePelicanSSE(strings.NewReader(input), func(chunk string) error { text.WriteString(chunk); return nil })
	if err != nil || text.String() != "HTML" {
		t.Fatalf("text=%q err=%v", text.String(), err)
	}
}

func TestPelicanJSONPartialAndRefusal(t *testing.T) {
	for _, data := range []string{
		`{"content":[{"type":"text","text":"partial"}],"stop_reason":"max_tokens"}`,
		`{"choices":[{"message":{"content":"partial"},"finish_reason":"length"}]}`,
		`{"output":[{"content":[{"type":"output_text","text":"partial"}]}],"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`,
	} {
		text, stop, err := extractPelicanJSON([]byte(data), strings.Contains(data, "stop_reason"))
		if err != nil || text != "partial" || pelicanStopError(stop) == nil {
			t.Fatalf("text=%q stop=%q err=%v", text, stop, err)
		}
	}
}

func TestPelicanWithoutDeadlineAndCancellation(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	service := newPelicanTestService(t, "codex", "/chat/completions", "bearer", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(writer, pelicanSSE(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": "partial"}}}}))
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
		cancelled <- struct{}{}
	})
	if service.client.Timeout != 0 {
		t.Fatalf("HTTP client has a timeout: %v", service.client.Timeout)
	}
	deadlines := make(chan bool, 1)
	service.client.Transport = pelicanTestTransport(func(request *http.Request) (*http.Response, error) {
		_, hasDeadline := request.Context().Deadline()
		deadlines <- hasDeadline
		return http.DefaultTransport.RoundTrip(request)
	})
	started, err := service.StartTest("codex", 42, "alias")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case hasDeadline := <-deadlines:
		if hasDeadline {
			t.Fatal("upstream request has a deadline")
		}
	case <-time.After(time.Second):
		t.Fatal("upstream request not started")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		result, _ := service.GetTest(started.SessionID)
		if result.RawOutput != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := service.CancelTest(started.SessionID); err != nil {
		t.Fatal(err)
	}
	result := awaitPelicanTest(t, service, started.SessionID)
	if result.ErrorCode != "cancelled" || result.RawOutput != "partial" {
		t.Fatalf("result = %+v", result)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("upstream request not cancelled")
	}
}

func TestPelicanLimitsAndStop(t *testing.T) {
	service := newPelicanTestService(t, "codex", "/chat/completions", "bearer", func(writer http.ResponseWriter, request *http.Request) { <-request.Context().Done() })
	for index := 0; index < pelicanMaxRunning; index++ {
		if _, err := service.StartTest("codex", 42, "alias"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.StartTest("codex", 42, "alias"); err == nil {
		t.Fatal("concurrency limit not enforced")
	}
	if err := service.Stop(); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	for _, session := range service.sessions {
		if session.result.Status != "cancelled" {
			t.Errorf("status = %s", session.result.Status)
		}
	}
	service.mu.Unlock()
	if _, err := service.StartTest("codex", 42, "alias"); err == nil {
		t.Fatal("stopped service accepted task")
	}
}

func TestPelicanOutputAndResponseLimits(t *testing.T) {
	for _, test := range []struct{ name, response, code string }{
		{"output", pelicanSSE(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": strings.Repeat("x", pelicanOutputLimit+1)}, "finish_reason": "stop"}}}), "output_limit"},
		{"response", strings.Repeat(":padding\n\n", pelicanResponseLimit/10+2), "response_limit"},
		{"large-event", "data: " + strings.Repeat("x", pelicanResponseLimit), "response_limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := newPelicanTestService(t, "codex", "/chat/completions", "bearer", func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(writer, test.response)
			})
			started, err := service.StartTest("codex", 42, "alias")
			if err != nil {
				t.Fatal(err)
			}
			result := awaitPelicanTest(t, service, started.SessionID)
			if result.ErrorCode != test.code || len(result.RawOutput) > pelicanOutputLimit {
				t.Fatalf("code=%s output=%d", result.ErrorCode, len(result.RawOutput))
			}
		})
	}
}

func TestPelicanRetentionAndValidation(t *testing.T) {
	service := NewPelicanTestService(nil)
	now := time.Now()
	service.sessions["expired"] = &pelicanSession{finished: now.Add(-pelicanRetention), result: PelicanTestResult{SessionID: "expired", Status: "completed", HTML: pelicanTestHTML, PreviewToken: "token"}}
	if _, err := service.GetTest("expired"); err == nil {
		t.Fatal("expired session available")
	}
	if _, ok := service.PreviewHTML("expired", "token"); ok {
		t.Fatal("expired preview available")
	}
	for index := 0; index < 21; index++ {
		id := fmt.Sprintf("session-%d", index)
		service.sessions[id] = &pelicanSession{finished: now.Add(time.Duration(index) * time.Millisecond), result: PelicanTestResult{SessionID: id, Status: "completed"}}
	}
	if _, err := service.GetTest("session-0"); err == nil || len(service.sessions) != pelicanMaxResults {
		t.Fatal("result count limit not enforced")
	}
	for _, model := range []string{"", "*", "model?", "model\nother"} {
		if _, err := service.StartTest("codex", 42, model); err == nil {
			t.Errorf("invalid model %q accepted", model)
		}
	}
	if _, err := service.StartTest("gemini", 42, "model"); err == nil {
		t.Fatal("unsupported platform accepted")
	}
	for _, platform := range []string{"custom:", "custom:../codex", "custom:..", "custom:test\\other"} {
		if _, err := service.StartTest(platform, 42, "model"); err == nil {
			t.Errorf("invalid custom platform %q accepted", platform)
		}
	}
	if err := service.CancelTest("missing"); err == nil {
		t.Fatal("unknown task cancellation accepted")
	}
}

func TestPelicanUTF8OutputLimit(t *testing.T) {
	service := NewPelicanTestService(nil)
	session := &pelicanSession{started: time.Now(), result: PelicanTestResult{RawOutput: strings.Repeat("x", pelicanOutputLimit-1)}}
	if err := service.appendText(session, "鹈鹕"); err == nil || len(session.result.RawOutput) != pelicanOutputLimit-1 {
		t.Fatal("invalid partial UTF-8 retained")
	}
}

func TestPelicanRedirectDoesNotRetryOrLeakCredentials(t *testing.T) {
	var calls atomic.Int32
	service := newPelicanTestService(t, "codex", "/chat/completions", "bearer", func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.Header().Set("Location", "/other")
		writer.WriteHeader(http.StatusTemporaryRedirect)
	})
	started, err := service.StartTest("codex", 42, "alias")
	if err != nil {
		t.Fatal(err)
	}
	result := awaitPelicanTest(t, service, started.SessionID)
	if result.ErrorCode != "http_error" || calls.Load() != 1 {
		t.Fatalf("result=%+v calls=%d", result, calls.Load())
	}
}
