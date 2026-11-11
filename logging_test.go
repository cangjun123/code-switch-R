package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestTruncatingHandler(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(truncatingHandler{slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})})

	logger.DebugContext(context.Background(), "xrequest", "curl", "secret-body")
	if buf.Len() != 0 {
		t.Fatalf("debug records must be dropped at info level: %q", buf.String())
	}

	huge := strings.Repeat("A", 5000)
	logger.Warn("xrequest", "response", huge, "status", 500)
	out := buf.String()
	if strings.Contains(out, strings.Repeat("A", maxLoggedAttrBytes+1)) || !strings.Contains(out, "truncated, 5000 bytes") || !strings.Contains(out, "status=500") {
		t.Fatalf("long attr not truncated: %q", out)
	}
}

func TestParseLogLevel(t *testing.T) {
	cases := map[string]slog.Level{"": slog.LevelInfo, "DEBUG": slog.LevelDebug, "warn": slog.LevelWarn, "error": slog.LevelError, "bogus": slog.LevelInfo}
	for raw, want := range cases {
		if got := parseLogLevel(raw); got != want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestRedactCredentialsInLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(truncatingHandler{slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})})
	curl := "curl -X 'POST' \\\n-d '{\"model\":\"m\"}' \\\n-H 'Authorization: Bearer sk-secret-123' \\\n-H 'x-api-key: xk-secret-456' \\\n-H 'X-Goog-Api-Key: AIza-secret' \\\n'https://host/v1beta/models?key=qk-secret&alt=sse'"
	logger.Warn("xrequest", "curl", curl, "error", "Authorization: Bearer sk-in-error")
	out := buf.String()
	for _, secret := range []string{"sk-secret-123", "xk-secret-456", "AIza-secret", "qk-secret", "sk-in-error"} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret %q leaked into log: %s", secret, out)
		}
	}
	if !strings.Contains(out, "curl=(omitted)") || !strings.Contains(out, "Bearer ***") {
		t.Fatalf("expected curl omitted and error redacted: %s", out)
	}
	if got := redactCredentials("GET https://h/v1beta/models?key=abc&alt=sse"); got != "GET https://h/v1beta/models?key=***&alt=sse" {
		t.Fatalf("query key not redacted: %s", got)
	}
}
