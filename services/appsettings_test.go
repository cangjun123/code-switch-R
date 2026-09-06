package services

import (
	"path/filepath"
	"testing"
	"time"
)

func newAppSettingsTestService(t *testing.T) *AppSettingsService {
	t.Helper()
	return &AppSettingsService{path: filepath.Join(t.TempDir(), "app.json")}
}

func TestCodexCapacityPreflightWaitDefaultsAndRoundTrip(t *testing.T) {
	as := newAppSettingsTestService(t)
	settings, err := as.GetAppSettings()
	if err != nil {
		t.Fatalf("GetAppSettings: %v", err)
	}
	if settings.CodexCapacityPreflightMaxWaitSec != CodexCapacityPreflightDefaultWaitSec {
		t.Fatalf("default wait = %d, want %d", settings.CodexCapacityPreflightMaxWaitSec, CodexCapacityPreflightDefaultWaitSec)
	}
	if got := as.CodexCapacityPreflightMaxWait(); got != 16*time.Second {
		t.Fatalf("getter wait = %s, want 16s", got)
	}

	settings.CodexCapacityPreflightMaxWaitSec = 30
	saved, err := as.SaveAppSettings(settings)
	if err != nil {
		t.Fatalf("SaveAppSettings: %v", err)
	}
	if saved.CodexCapacityPreflightMaxWaitSec != 30 {
		t.Fatalf("saved wait = %d, want 30", saved.CodexCapacityPreflightMaxWaitSec)
	}
	if got := as.CodexCapacityPreflightMaxWait(); got != 30*time.Second {
		t.Fatalf("getter wait = %s, want 30s", got)
	}
}

func TestCodexCapacityPreflightWaitClampsInvalidValues(t *testing.T) {
	as := newAppSettingsTestService(t)
	settings := as.defaultSettings()
	settings.CodexCapacityPreflightMaxWaitSec = 1
	saved, err := as.SaveAppSettings(settings)
	if err != nil {
		t.Fatalf("SaveAppSettings low value: %v", err)
	}
	if saved.CodexCapacityPreflightMaxWaitSec != CodexCapacityPreflightMinWaitSec {
		t.Fatalf("low value saved as %d, want min %d", saved.CodexCapacityPreflightMaxWaitSec, CodexCapacityPreflightMinWaitSec)
	}

	settings.CodexCapacityPreflightMaxWaitSec = 999
	saved, err = as.SaveAppSettings(settings)
	if err != nil {
		t.Fatalf("SaveAppSettings high value: %v", err)
	}
	if saved.CodexCapacityPreflightMaxWaitSec != CodexCapacityPreflightMaxWaitSec {
		t.Fatalf("high value saved as %d, want max %d", saved.CodexCapacityPreflightMaxWaitSec, CodexCapacityPreflightMaxWaitSec)
	}
	if got := as.CodexCapacityPreflightMaxWait(); got != 120*time.Second {
		t.Fatalf("getter wait = %s, want 120s", got)
	}
}
