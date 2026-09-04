package services

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexTraceStagesAndSpans(t *testing.T) {
	trace := &codexTrace{requestID: "test", start: time.Now().Add(-3 * time.Second)}
	trace.mark("auth_done")
	trace.mark("quota_check_start")
	time.Sleep(5 * time.Millisecond)
	trace.mark("quota_check_done")
	trace.markOnce("first_client_write")
	trace.markOnce("first_client_write") // 幂等：不应重复记录

	if got := trace.elapsedOf("auth_done"); got < 2900 {
		t.Fatalf("auth_done elapsed=%v, want >= 2900", got)
	}
	quotaWait := trace.spanOf("quota_check_start", "quota_check_done")
	if quotaWait < 0 {
		t.Fatal("quota span missing")
	}
	count := 0
	trace.mu.Lock()
	for _, stage := range trace.stages {
		if stage.name == "first_client_write" {
			count++
		}
	}
	trace.mu.Unlock()
	if count != 1 {
		t.Fatalf("first_client_write recorded %d times, want 1", count)
	}

	trace.setKeyID("key-1")
	trace.setModel("gpt-test")
	trace.setProvider("prov-1")
	trace.setBodyBytes(4096)
	trace.setStatus(200)
	trace.recordAttempt(codexTraceAttempt{attemptID: "a1", dnsMs: -1, connectMs: -1, tlsMs: -1, writeMs: 120, headersMs: 9500})

	if got := trace.keyID; got != "key-1" || trace.model != "gpt-test" || trace.provider != "prov-1" || trace.httpStatus != 200 {
		t.Fatalf("scalar fields not stored: %+v", trace)
	}
	attempt, ok := trace.lastAttempt()
	if !ok || attempt.attemptID != "a1" || attempt.writeMs != 120 {
		t.Fatalf("attempt not stored: %+v ok=%v", attempt, ok)
	}
}

func TestCodexTraceFormatTraceMs(t *testing.T) {
	if formatTraceMs(-1) != "-" {
		t.Fatal("negative duration should render as -")
	}
	if formatTraceMs(0) != "0" {
		t.Fatal("zero duration should render as 0")
	}
}

func TestAppSettingsIsCodexTraceEnabledMtimeCache(t *testing.T) {
	dir := t.TempDir()
	service := &AppSettingsService{path: filepath.Join(dir, "app.json")}

	if service.IsCodexTraceEnabled() {
		t.Fatal("missing settings file should default to disabled")
	}

	write := func(enabled bool) {
		value := "false"
		if enabled {
			value = "true"
		}
		if err := os.WriteFile(service.path, []byte(`{"codex_trace_enabled":`+value+`}`), 0o644); err != nil {
			t.Fatalf("write settings: %v", err)
		}
		// mtime 粒度：确保 Stat 能观察到变化。
		future := time.Now().Add(2 * time.Second)
		if err := os.Chtimes(service.path, future, future); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	write(true)
	if !service.IsCodexTraceEnabled() {
		t.Fatal("settings update should be picked up after mtime change")
	}
	write(false)
	if service.IsCodexTraceEnabled() {
		t.Fatal("disable should be picked up after mtime change")
	}
}

func TestCodexTraceEnabledEnvOverride(t *testing.T) {
	t.Setenv("CODESWITCH_CODEX_TRACE", "1")
	if !codexTraceEnabled(nil) {
		t.Fatal("env var should force tracing on even without settings service")
	}
	t.Setenv("CODESWITCH_CODEX_TRACE", "0")
	if codexTraceEnabled(nil) {
		t.Fatal("env var 0 should not enable tracing")
	}
}

func TestCodexTraceFromContextLookup(t *testing.T) {
	if codexTraceFromContext(context.Background()) != nil {
		t.Fatal("empty context should not carry a trace")
	}
	trace := &codexTrace{requestID: "ctx"}
	ctx := context.WithValue(context.Background(), codexTraceContextKey{}, trace)
	if got := codexTraceRequestID(ctx); got != "ctx" {
		t.Fatalf("requestID=%q, want ctx", got)
	}
	markCodexPreflightFirstEvent(ctx)
	if trace.elapsedOf("first_upstream_event") < 0 {
		t.Fatal("first event stage missing after mark")
	}
	if len(newCodexTraceID()) == 0 {
		t.Fatal("trace id should not be empty")
	}
}
