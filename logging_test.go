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
	logger.Warn("xrequest", "curl", huge, "status", 500)
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
