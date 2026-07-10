package services

import (
	"bytes"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/daodao97/xgo/xdb"
	"github.com/tidwall/gjson"
)

func setNamespaceRoutingDBSetting(t *testing.T, key, value string) {
	t.Helper()
	db, err := xdb.DB("default")
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}

	var previous string
	err = db.QueryRow(`SELECT value FROM app_settings WHERE key = ?`, key).Scan(&previous)
	existed := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("read app setting %q: %v", key, err)
	}
	if _, err := db.Exec(`
		INSERT INTO app_settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value); err != nil {
		t.Fatalf("set app setting %q: %v", key, err)
	}

	t.Cleanup(func() {
		if existed {
			if _, err := db.Exec(`UPDATE app_settings SET value = ? WHERE key = ?`, previous, key); err != nil {
				t.Errorf("restore app setting %q: %v", key, err)
			}
			return
		}
		if _, err := db.Exec(`DELETE FROM app_settings WHERE key = ?`, key); err != nil {
			t.Errorf("delete test app setting %q: %v", key, err)
		}
	})
}

func setNamespaceRoutingRetryWait(t *testing.T, settings *SettingsService, seconds int) {
	t.Helper()
	path, err := GetBlacklistLevelConfigPath()
	if err != nil {
		t.Fatalf("GetBlacklistLevelConfigPath: %v", err)
	}
	previous, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read blacklist config: %v", readErr)
	}

	config, err := settings.GetBlacklistLevelConfig()
	if err != nil {
		t.Fatalf("GetBlacklistLevelConfig: %v", err)
	}
	config.RetryWaitSeconds = seconds
	if err := settings.SaveBlacklistLevelConfig(config); err != nil {
		t.Fatalf("SaveBlacklistLevelConfig: %v", err)
	}

	t.Cleanup(func() {
		if readErr == nil {
			if err := os.WriteFile(path, previous, 0o644); err != nil {
				t.Errorf("restore blacklist config: %v", err)
			}
			return
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove test blacklist config: %v", err)
		}
	})
}

func clearNamespaceRoutingBlacklistRows(t *testing.T, providerNames ...string) {
	t.Helper()
	db, err := xdb.DB("default")
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	clear := func() {
		for _, name := range providerNames {
			if _, err := db.Exec(`DELETE FROM provider_blacklist WHERE platform = ? AND provider_name = ?`, ProviderKindCodex, name); err != nil {
				t.Errorf("clear blacklist row for %q: %v", name, err)
			}
		}
	}
	clear()
	t.Cleanup(clear)
}

func TestCodexMultiAgentNamespaceFixedBlacklistRetryKeepsProviderBodiesIsolated(t *testing.T) {
	const (
		rewriteName = "namespace-fixed-rewrite-routing-test"
		plainName   = "namespace-fixed-plain-routing-test"
	)
	requestBody := []byte(`{"model":"gpt-5-codex","tools":[{"type":"namespace","name":"collaboration"}],"input":[{"type":"function_call","namespace":"collaboration","name":"spawn_agent"}]}`)

	var mu sync.Mutex
	var rewriteBodies [][]byte
	rewriteUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		rewriteBodies = append(rewriteBodies, append([]byte(nil), body...))
		mu.Unlock()
		http.Error(w, "retry this provider", http.StatusInternalServerError)
	}))
	defer rewriteUpstream.Close()

	var plainBody []byte
	plainUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"type":"function_call","namespace":"agents","name":"spawn_agent"}]}`))
	}))
	defer plainUpstream.Close()

	providers, relay := newTestRelayService(t)
	if GlobalDBQueue == nil {
		if err := InitGlobalDBQueue(); err != nil {
			t.Fatalf("InitGlobalDBQueue: %v", err)
		}
	}
	setNamespaceRoutingDBSetting(t, "enable_blacklist", "true")
	setNamespaceRoutingDBSetting(t, "blacklist_level_enabled", "false")
	setNamespaceRoutingDBSetting(t, "blacklist_failure_threshold", "2")
	setNamespaceRoutingRetryWait(t, relay.blacklistService.settingsService, 0)
	clearNamespaceRoutingBlacklistRows(t, rewriteName, plainName)

	if err := providers.SaveProviders(ProviderKindCodex, []Provider{
		{ID: 1, Name: rewriteName, APIURL: rewriteUpstream.URL, APIKey: "rewrite-key", Enabled: true, Level: 1, CodexMultiAgentNamespaceRewrite: true},
		{ID: 2, Name: plainName, APIURL: plainUpstream.URL, APIKey: "plain-key", Enabled: true, Level: 2},
	}); err != nil {
		t.Fatalf("SaveProviders: %v", err)
	}

	recorder := performCodexNamespaceTestRequest(t, relay, requestBody)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	mu.Lock()
	attemptBodies := append([][]byte(nil), rewriteBodies...)
	mu.Unlock()
	if len(attemptBodies) != 2 {
		t.Fatalf("rewrite provider attempts = %d, want 2", len(attemptBodies))
	}
	for i, body := range attemptBodies {
		if got := gjson.GetBytes(body, "tools.0.name").String(); got != "agents" {
			t.Fatalf("rewrite attempt %d tool namespace = %q, body = %s", i+1, got, body)
		}
		if got := gjson.GetBytes(body, "input.0.namespace").String(); got != "agents" {
			t.Fatalf("rewrite attempt %d history namespace = %q, body = %s", i+1, got, body)
		}
	}
	if !bytes.Equal(plainBody, requestBody) {
		t.Fatalf("plain fallback received polluted body:\n got: %s\nwant: %s", plainBody, requestBody)
	}
	if got := gjson.Get(recorder.Body.String(), "output.0.namespace").String(); got != "agents" {
		t.Fatalf("plain fallback response was reverse-rewritten: %q", got)
	}
	if blacklisted, _ := relay.blacklistService.IsBlacklisted(ProviderKindCodex, rewriteName); !blacklisted {
		t.Fatal("rewrite provider was not blacklisted after two failed attempts")
	}
}

func TestCodexMultiAgentNamespaceRoundRobinAppliesPerProviderModelMapping(t *testing.T) {
	const requestedModel = "namespace-client-model"
	requestBody := []byte(`{"model":"namespace-client-model","tools":[{"type":"namespace","name":"collaboration"}],"input":[{"type":"function_call","namespace":"collaboration","name":"spawn_agent"}]}`)

	var mu sync.Mutex
	var rewriteBodies, plainBodies [][]byte
	rewriteUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		rewriteBodies = append(rewriteBodies, append([]byte(nil), body...))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"type":"function_call","namespace":"agents","name":"spawn_agent"}]}`))
	}))
	defer rewriteUpstream.Close()
	plainUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		plainBodies = append(plainBodies, append([]byte(nil), body...))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"type":"function_call","namespace":"agents","name":"spawn_agent"}]}`))
	}))
	defer plainUpstream.Close()

	providers, relay := newTestRelayService(t)
	setNamespaceRoutingDBSetting(t, "enable_blacklist", "false")
	settings, err := relay.appSettings.GetAppSettings()
	if err != nil {
		t.Fatalf("GetAppSettings: %v", err)
	}
	previousSettings := settings
	settings.EnableRoundRobin = true
	settings.CodexDegradationResendEnabled = false
	if _, err := relay.appSettings.SaveAppSettings(settings); err != nil {
		t.Fatalf("enable round robin: %v", err)
	}
	t.Cleanup(func() {
		if _, err := relay.appSettings.SaveAppSettings(previousSettings); err != nil {
			t.Errorf("restore app settings: %v", err)
		}
	})

	if err := providers.SaveProviders(ProviderKindCodex, []Provider{
		{
			ID:                              1,
			Name:                            "namespace-round-robin-rewrite-test",
			APIURL:                          rewriteUpstream.URL,
			APIKey:                          "rewrite-key",
			Enabled:                         true,
			Level:                           1,
			ModelMapping:                    map[string]string{requestedModel: "rewrite-upstream-model"},
			CodexMultiAgentNamespaceRewrite: true,
		},
		{
			ID:           2,
			Name:         "namespace-round-robin-plain-test",
			APIURL:       plainUpstream.URL,
			APIKey:       "plain-key",
			Enabled:      true,
			Level:        1,
			ModelMapping: map[string]string{requestedModel: "plain-upstream-model"},
		},
	}); err != nil {
		t.Fatalf("SaveProviders: %v", err)
	}

	first := performCodexNamespaceTestRequest(t, relay, requestBody)
	second := performCodexNamespaceTestRequest(t, relay, requestBody)
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("statuses = (%d, %d), bodies = (%s, %s)", first.Code, second.Code, first.Body.String(), second.Body.String())
	}

	mu.Lock()
	rewritten := append([][]byte(nil), rewriteBodies...)
	plain := append([][]byte(nil), plainBodies...)
	mu.Unlock()
	if len(rewritten) != 1 || len(plain) != 1 {
		t.Fatalf("round robin hits rewrite=%d plain=%d, want one each", len(rewritten), len(plain))
	}
	if got := gjson.GetBytes(rewritten[0], "model").String(); got != "rewrite-upstream-model" {
		t.Fatalf("rewrite provider model = %q, body = %s", got, rewritten[0])
	}
	if got := gjson.GetBytes(rewritten[0], "tools.0.name").String(); got != "agents" {
		t.Fatalf("rewrite provider tool namespace = %q, body = %s", got, rewritten[0])
	}
	if got := gjson.GetBytes(rewritten[0], "input.0.namespace").String(); got != "agents" {
		t.Fatalf("rewrite provider history namespace = %q, body = %s", got, rewritten[0])
	}

	wantPlainBody, err := ReplaceModelInRequestBody(requestBody, "plain-upstream-model")
	if err != nil {
		t.Fatalf("build expected plain body: %v", err)
	}
	if !bytes.Equal(plain[0], wantPlainBody) {
		t.Fatalf("plain provider body was not derived from original request:\n got: %s\nwant: %s", plain[0], wantPlainBody)
	}
	if got := gjson.Get(first.Body.String(), "output.0.namespace").String(); got != "collaboration" {
		t.Fatalf("rewrite provider client response namespace = %q", got)
	}
	if got := gjson.Get(second.Body.String(), "output.0.namespace").String(); got != "agents" {
		t.Fatalf("plain provider client response namespace = %q", got)
	}
}
