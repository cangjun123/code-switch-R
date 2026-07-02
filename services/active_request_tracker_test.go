package services

import (
	"testing"
	"time"

	"github.com/daodao97/xgo/xdb"
)

func TestActiveRequestTrackerListFiltersAndFinishes(t *testing.T) {
	tracker := newActiveRequestTracker()
	now := time.Now()

	oldID := tracker.Start(&ReqeustLog{
		Platform: "claude",
		Provider: "provider-a",
		Model:    "claude-sonnet",
		IsStream: true,
		ClientIP: "127.0.0.1",
	}, now.Add(-3*time.Second))
	newID := tracker.Start(&ReqeustLog{
		Platform: "codex",
		Provider: "provider-b",
		Model:    "gpt-4.1",
	}, now.Add(-time.Second))

	logs := tracker.List("", "", time.UTC)
	if len(logs) != 2 {
		t.Fatalf("active logs count = %d, want 2", len(logs))
	}
	if logs[0].Provider != "provider-b" || logs[1].Provider != "provider-a" {
		t.Fatalf("active logs order = [%s, %s], want newest first", logs[0].Provider, logs[1].Provider)
	}
	if logs[0].ID >= 0 || logs[0].Status != requestLogStatusProcessing {
		t.Fatalf("active log metadata = id %d status %q, want negative processing", logs[0].ID, logs[0].Status)
	}
	if logs[1].DurationSec <= 0 {
		t.Fatalf("active duration = %f, want positive", logs[1].DurationSec)
	}

	tracker.Update(oldID, &ReqeustLog{
		Platform: "claude",
		Provider: "provider-c",
		Model:    "claude-opus",
	})
	filtered := tracker.List("claude", "provider-c", time.UTC)
	if len(filtered) != 1 || filtered[0].Model != "claude-opus" {
		t.Fatalf("filtered active logs = %#v, want updated claude provider-c", filtered)
	}

	tracker.Finish(oldID)
	tracker.Finish(newID)
	if remaining := tracker.List("", "", time.UTC); len(remaining) != 0 {
		t.Fatalf("remaining active logs = %d, want 0", len(remaining))
	}
}

func TestListRequestLogsPrependsActiveRequests(t *testing.T) {
	setupRelayTestEnv(t)

	previousTracker := defaultActiveRequestTracker
	defaultActiveRequestTracker = newActiveRequestTracker()
	t.Cleanup(func() {
		defaultActiveRequestTracker = previousTracker
	})

	db, err := xdb.DB("default")
	if err != nil {
		t.Fatalf("get db: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM request_log`); err != nil {
		t.Fatalf("clear request_log: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO request_log (
			platform, model, provider, http_code,
			input_tokens, output_tokens, cache_create_tokens, cache_read_tokens,
			reasoning_tokens, is_stream, duration_sec, first_token_duration_sec, client_ip,
			is_degraded, resend_count
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, "claude", "claude-sonnet", "completed-provider", 200, 10, 20, 0, 0, 0, 1, 2.5, 0.4, "127.0.0.1", 0, 0); err != nil {
		t.Fatalf("insert request_log: %v", err)
	}

	activeID := defaultActiveRequestTracker.Start(&ReqeustLog{
		Platform: "claude",
		Provider: "active-provider",
		Model:    "claude-opus",
		IsStream: true,
		ClientIP: "127.0.0.2",
	}, time.Now().Add(-time.Second))
	defer defaultActiveRequestTracker.Finish(activeID)

	logs, err := NewLogService().ListRequestLogs("claude", "", 10, "UTC")
	if err != nil {
		t.Fatalf("ListRequestLogs: %v", err)
	}
	if len(logs) < 2 {
		t.Fatalf("logs count = %d, want at least 2", len(logs))
	}
	if logs[0].Status != requestLogStatusProcessing || logs[0].Provider != "active-provider" {
		t.Fatalf("first log = status %q provider %q, want processing active-provider", logs[0].Status, logs[0].Provider)
	}
	if logs[1].Status != requestLogStatusCompleted || logs[1].Provider != "completed-provider" {
		t.Fatalf("second log = status %q provider %q, want completed completed-provider", logs[1].Status, logs[1].Provider)
	}
}
