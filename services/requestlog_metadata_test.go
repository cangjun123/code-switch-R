package services

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestResponseModelParsers(t *testing.T) {
	cases := []struct {
		name, body, want string
		parse            func(string, *ReqeustLog)
	}{
		{"claude JSON", `{"model":"claude-returned","usage":{"input_tokens":1}}`, "claude-returned", ClaudeCodeParseTokenUsageFromResponse},
		{"claude SSE start without usage", `{"type":"message_start","message":{"model":"claude-returned"}}`, "claude-returned", ClaudeCodeParseTokenUsageFromResponse},
		{"responses JSON", `{"model":"gpt-returned"}`, "gpt-returned", CodexParseTokenUsageFromResponse},
		{"responses SSE", `{"type":"response.created","response":{"model":"gpt-returned"}}`, "gpt-returned", CodexParseTokenUsageFromResponse},
		{"chat SSE", `{"model":"gpt-chat","choices":[{"delta":{"content":"hi"}}]}`, "gpt-chat", CodexParseTokenUsageFromResponse},
		{"gemini without usage", `{"modelVersion":"gemini-version"}`, "gemini-version", GeminiParseTokenUsageFromResponse},
		{"missing", `{"usage":{"input_tokens":1}}`, "", CodexParseTokenUsageFromResponse},
		{"invalid model type", `{"model":123}`, "", CodexParseTokenUsageFromResponse},
		{"content is not metadata", `{"choices":[{"message":{"content":"model: fake","model":"fake"}}]}`, "", CodexParseTokenUsageFromResponse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := &ReqeustLog{Model: "upstream-request", RequestedModel: "client-request"}
			tc.parse(tc.body, entry)
			tc.parse(`{"usage":{"output_tokens":2}}`, entry)
			if entry.ResponseModel != tc.want || entry.Model != "upstream-request" || entry.RequestedModel != "client-request" {
				t.Fatalf("unexpected models: requested=%q upstream=%q response=%q", entry.RequestedModel, entry.Model, entry.ResponseModel)
			}
		})
	}
}

func TestConvertedStreamRecordsOnlyUpstreamModel(t *testing.T) {
	for _, model := range []string{"", "actual-upstream"} {
		for _, protocol := range []string{"chat", "responses"} {
			t.Run(protocol+"/"+model, func(t *testing.T) {
				entry := &ReqeustLog{Model: "mapped-request", RequestedModel: "client-request"}
				id := defaultActiveRequestTracker.Start(entry, time.Now())
				entry.ActiveRequestID = id
				defer defaultActiveRequestTracker.Finish(id)
				modelField := ""
				if model != "" {
					modelField = `,"model":"` + model + `"`
				}
				var converter SSEProtocolConverter
				var payload string
				if protocol == "chat" {
					converter = NewOpenAIToAnthropicSSEConverter("synthetic-model")
					payload = `data: {"id":"chat1"` + modelField + `,"choices":[{"index":0,"delta":{"content":"hi"}}]}`
				} else {
					converter = NewResponsesToAnthropicSSEConverter("synthetic-model")
					payload = `data: {"type":"response.created","response":{"id":"resp1"` + modelField + `}}`
				}
				protocolConvertHook(converter, "claude", entry)([]byte(payload))
				if entry.ResponseModel != model {
					t.Fatalf("response model = %q, want %q", entry.ResponseModel, model)
				}
				for _, active := range defaultActiveRequestTracker.List("", "", time.UTC) {
					if active.ID == -id && active.ResponseModel != model {
						t.Fatalf("active log contains synthetic model: %q", active.ResponseModel)
					}
				}
			})
		}
	}
}

func TestGeminiAndImageStreamModelCapture(t *testing.T) {
	geminiBody := "data: {\"modelVersion\":\"gemini-returned\"}\r\n\r\ndata: {\"usageMetadata\":{\"promptTokenCount\":3}}\n\n"
	entry := &ReqeustLog{}
	var forwarded bytes.Buffer
	if err := streamGeminiResponseWithHook(strings.NewReader(geminiBody), &forwarded, entry, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if entry.ResponseModel != "gemini-returned" || forwarded.String() != geminiBody {
		t.Fatalf("Gemini metadata/body incorrect: %+v", entry)
	}
	parseGeminiUsageMetadata([]byte(`{"modelVersion":"gemini-json"}`), entry)
	if entry.ResponseModel != "gemini-json" {
		t.Fatal("missing nonstreaming Gemini model")
	}

	imageBody := "data: {\"model\":\"image-returned\"}\n\ndata: [DONE]"
	entry = &ReqeustLog{}
	rec := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(imageBody))}
	if err := streamOpenAIImageResponse(rec, resp, true, entry, time.Now()); err != nil {
		t.Fatal(err)
	}
	if entry.ResponseModel != "image-returned" || rec.Body.String() != imageBody {
		t.Fatalf("image metadata/body incorrect: %+v", entry)
	}
	observer := newResponseModelObserver(entry)
	observer.Write([]byte("data: {\"mod"))
	observer.Write([]byte("el\":\"split-image\"}"))
	observer.Finish()
	if entry.ResponseModel != "split-image" {
		t.Fatal("missing chunked final model")
	}
}

func TestForwardRequestPersistsModelAndKeyMetadata(t *testing.T) {
	setupRelayTestEnv(t)
	if GlobalDBQueueLogs == nil {
		if err := InitGlobalDBQueue(); err != nil {
			t.Fatal(err)
		}
	}
	for _, stream := range []bool{false, true} {
		name := "metadata-json"
		upstreamBody := `{"model":"actual-returned","usage":{"input_tokens":3,"output_tokens":1}}`
		contentType := "application/json"
		if stream {
			name = "metadata-sse"
			upstreamBody = "data: {\"type\":\"message_start\",\"message\":{\"model\":\"actual-returned\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
			contentType = "text/event-stream"
		}
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", contentType)
				_, _ = io.WriteString(w, upstreamBody)
			}))
			defer upstream.Close()
			prs := &ProviderRelayService{httpClient: newRelayHTTPClient()}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			body := []byte(`{"model":"mapped-request","messages":[],"max_tokens":10}`)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			c.Set(requestedModelContextKey, "client-request")
			setRelayKeyContext(c, &CodexRelayKey{ID: "test-key-id", Name: "test key name", Key: "secret-must-not-appear"})
			ok, err := prs.forwardRequest(c, "claude", Provider{Name: name, APIURL: upstream.URL}, "/v1/messages", nil, map[string]string{"Content-Type": "application/json"}, body, stream, "mapped-request")
			if err != nil || !ok {
				t.Fatalf("forward failed: %v", err)
			}
			if rec.Body.String() != upstreamBody {
				t.Fatal("response body changed")
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				logs, err := NewLogService().ListRequestLogs("claude", name, 10, "UTC")
				if err != nil {
					t.Fatal(err)
				}
				if len(logs) > 0 {
					got := logs[0]
					if got.RequestedModel != "client-request" || got.Model != "mapped-request" || got.ResponseModel != "actual-returned" || got.RelayKeyID != "test-key-id" || got.RelayKeyName != "test key name" {
						t.Fatalf("metadata roundtrip failed: %+v", got)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("timed out waiting for persisted metadata")
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}
