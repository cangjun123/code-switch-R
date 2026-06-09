package services

import (
	"testing"
	"time"
)

func TestParseTimeInputUsesProvidedLocationForNaiveTime(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}

	parsed, err := parseTimeInput("2026-06-09 00:00:00", loc)
	if err != nil {
		t.Fatalf("parseTimeInput: %v", err)
	}

	if got := parsed.Location().String(); got != "Asia/Shanghai" {
		t.Fatalf("location = %s, want Asia/Shanghai", got)
	}
	if got := parsed.UTC().Format(timeLayout); got != "2026-06-08 16:00:00" {
		t.Fatalf("UTC time = %s, want 2026-06-08 16:00:00", got)
	}
}

func TestParseStoredLogTimeTreatsSQLiteTimestampAsUTC(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}

	parsed, err := parseStoredLogTime("2026-06-08 16:00:00")
	if err != nil {
		t.Fatalf("parseStoredLogTime: %v", err)
	}

	if got := parsed.In(loc).Format(timeLayout); got != "2026-06-09 00:00:00" {
		t.Fatalf("local time = %s, want 2026-06-09 00:00:00", got)
	}
}

func TestResolveLogLocationFallsBackToLocal(t *testing.T) {
	if got := resolveLogLocation("bad/timezone"); got != time.Local {
		t.Fatalf("fallback location = %v, want time.Local", got)
	}
}
