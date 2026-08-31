package services

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/daodao97/xgo/xdb"
	"github.com/daodao97/xgo/xrequest"
)

type quotaFakeRoundTripper struct {
	mu        sync.Mutex
	responses []string
	calls     int
}

func (r *quotaFakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	index := r.calls - 1
	if index >= len(r.responses) {
		index = len(r.responses) - 1
	}
	body := "{}"
	if index >= 0 {
		body = r.responses[index]
	}
	contentType := "application/json"
	if strings.Contains(body, "data:") {
		contentType = "text/event-stream"
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func (r *quotaFakeRoundTripper) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func consumeQuotaResponse(t *testing.T, response *xrequest.Response) {
	t.Helper()
	if response == nil || response.RawResponse == nil || response.RawResponse.Body == nil {
		t.Fatal("missing fake upstream response body")
	}
	if _, err := io.ReadAll(response.RawResponse.Body); err != nil {
		t.Fatalf("consume response body: %v", err)
	}
	_ = response.RawResponse.Body.Close()
}

func TestCodexQuotaTransportSettlesEachAttemptOnceAndParsesSSE(t *testing.T) {
	setupRelayTestEnv(t)
	keyService := &CodexRelayKeyService{path: t.TempDir() + "/keys.json"}
	created, err := keyService.CreateKey("quota-transport-test")
	if err != nil {
		t.Fatalf("create test key: %v", err)
	}
	if err := keyService.UpdateQuotaConfig(created.ID, 1_000_000, "0", RelayQuotaPeriodOnce); err != nil {
		t.Fatalf("configure test key: %v", err)
	}
	key, err := keyService.GetKeyByID(created.ID)
	if err != nil {
		t.Fatalf("reload test key: %v", err)
	}
	quota := NewRelayQuotaService()
	if _, err := quota.UpsertModelPrice(RelayModelPrice{
		Model: "transport-model", Input: "1", CachedInput: "0", Output: "2", ReasoningOutput: "4",
	}); err != nil {
		t.Fatalf("configure model price: %v", err)
	}
	defer func() {
		db, _ := xdb.DB("default")
		if db != nil {
			_, _ = db.Exec("DELETE FROM relay_key_quota_state WHERE relay_key_id = ?", key.ID)
			_, _ = db.Exec("DELETE FROM relay_key_quota_settlement WHERE relay_key_id = ?", key.ID)
			_, _ = db.Exec("DELETE FROM relay_model_price WHERE model = ?", "transport-model")
		}
	}()

	sse := "data: {\"type\":\"response.output_text.delta\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":8,\"input_tokens_details\":{\"cached_tokens\":2},\"output_tokens_details\":{\"reasoning_tokens\":3}}}}\n\n"
	transport := &quotaFakeRoundTripper{responses: []string{sse, sse}}
	relay := &ProviderRelayService{
		httpClient:     &http.Client{Transport: transport},
		codexRelayKeys: keyService,
		relayQuota:     quota,
	}
	ctx := withCodexQuotaAttemptTracker(context.Background(), &codexQuotaAttemptTracker{
		service: relay, keyID: key.ID, provider: "fake", model: "transport-model",
	})
	for i := 0; i < 2; i++ {
		response, err := relay.postCodexResponsesRequest(ctx, "http://quota.invalid/responses", nil,
			map[string]string{"Content-Type": "application/json"}, []byte(`{"model":"transport-model"}`), "fake")
		if err != nil {
			t.Fatalf("post attempt %d: %v", i+1, err)
		}
		consumeQuotaResponse(t, response)
		// Close again to verify Read(EOF)+Close remains idempotent.
		_ = response.RawResponse.Body.Close()
	}
	if transport.callCount() != 2 {
		t.Fatalf("fake transport calls=%d, want 2", transport.callCount())
	}
	status, err := quota.Status(key)
	if err != nil {
		t.Fatalf("quota status: %v", err)
	}
	if status.TokenUsed != 40 || status.Usage.CachedInputTokens != 4 || status.Usage.ReasoningOutputTokens != 6 {
		t.Fatalf("unexpected accumulated SSE usage: %+v", status)
	}
	db, err := xdb.DB("default")
	if err != nil {
		t.Fatalf("quota db: %v", err)
	}
	var settlements int
	if err := db.QueryRow("SELECT COUNT(*) FROM relay_key_quota_settlement WHERE relay_key_id = ?", key.ID).Scan(&settlements); err != nil {
		t.Fatalf("settlement count: %v", err)
	}
	if settlements != 2 {
		t.Fatalf("settlement rows=%d, want 2", settlements)
	}
}

func TestCodexQuotaTrackingBodyKeepsBoundedMemory(t *testing.T) {
	body := io.NopCloser(strings.NewReader(strings.Repeat("x", codexQuotaTrackingMaxBufferBytes*3)))
	tracking := &codexQuotaTrackingBody{ReadCloser: body, attemptID: "bounded-test"}
	if _, err := io.Copy(io.Discard, tracking); err != nil {
		t.Fatalf("consume large body: %v", err)
	}
	tracking.mu.Lock()
	buffered := len(tracking.buffer)
	tracking.mu.Unlock()
	if buffered > codexQuotaTrackingMaxBufferBytes {
		t.Fatalf("tracking buffer=%d exceeds bound %d", buffered, codexQuotaTrackingMaxBufferBytes)
	}
}
