package services

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestImportCcSwitchGeminiProvidersFromJSON(t *testing.T) {
	providerService, gemini := newGeminiAvailabilityEnv(t, GeminiProvider{ID: "existing", Name: "Existing", BaseURL: "https://existing.test"})
	importService := NewImportService(providerService, nil)
	importService.SetGeminiService(gemini)

	config := map[string]any{
		"gemini": map[string]any{"providers": map[string]any{
			"p1":    map[string]any{"name": "Relay G", "websiteUrl": "https://relay.test", "settingsConfig": map[string]any{"env": map[string]any{"GOOGLE_GEMINI_BASE_URL": "https://relay.test", "GEMINI_API_KEY": "gk", "GEMINI_MODEL": "gemini-2.5-pro"}}},
			"dup":   map[string]any{"name": "Dup", "settingsConfig": map[string]any{"env": map[string]any{"GOOGLE_GEMINI_BASE_URL": "https://existing.test/", "GEMINI_API_KEY": "x"}}},
			"oauth": map[string]any{"name": "Google Official", "settingsConfig": map[string]any{"env": map[string]any{}}},
		}},
	}
	data, _ := json.Marshal(config)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := importService.ImportFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.ImportedProviders != 1 {
		t.Fatalf("expected 1 imported Gemini provider, got %d", result.ImportedProviders)
	}
	var imported *GeminiProvider
	for _, p := range gemini.GetProviders() {
		if p.Name == "Relay G" {
			copy := p
			imported = &copy
		}
	}
	if imported == nil || imported.BaseURL != "https://relay.test" || imported.APIKey != "gk" || imported.Model != "gemini-2.5-pro" || imported.NumericID == 0 {
		t.Fatalf("unexpected imported provider: %+v", imported)
	}
	// 再次导入不重复
	if result, err := importService.ImportFromPath(path); err != nil || result.ImportedProviders != 0 {
		t.Fatalf("re-import should be a no-op: %+v %v", result, err)
	}
}

func TestImportCcSwitchGeminiProvidersFromSQLite(t *testing.T) {
	providerService, gemini := newGeminiAvailabilityEnv(t)
	importService := NewImportService(providerService, nil)
	importService.SetGeminiService(gemini)

	path := filepath.Join(t.TempDir(), "cc-switch.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE providers (id TEXT, app_type TEXT, name TEXT, settings_config TEXT, website_url TEXT)`,
		`CREATE TABLE provider_endpoints (provider_id TEXT, app_type TEXT, url TEXT)`,
		`INSERT INTO providers VALUES ('g1', 'gemini', 'SQLite Gemini', '{"env":{"GEMINI_API_KEY":"sk"}}', '')`,
		`INSERT INTO provider_endpoints VALUES ('g1', 'gemini', 'https://sqlite-gemini.test')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()

	result, err := importService.ImportFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	providers := gemini.GetProviders()
	if result.ImportedProviders != 1 || len(providers) != 1 || providers[0].BaseURL != "https://sqlite-gemini.test" || providers[0].APIKey != "sk" {
		t.Fatalf("SQLite Gemini import failed: %+v %+v", result, providers)
	}
	claude, _ := providerService.LoadProviders(ProviderKindClaude)
	for _, p := range claude {
		if p.Name == "SQLite Gemini" {
			t.Fatal("Gemini row must not be imported as a Claude provider")
		}
	}
}

func TestDeepLinkImportGeminiProvider(t *testing.T) {
	providerService, gemini := newGeminiAvailabilityEnv(t)
	deeplink := NewDeepLinkService(providerService)
	deeplink.SetGeminiService(gemini)

	config := base64.StdEncoding.EncodeToString([]byte(`{"GOOGLE_GEMINI_BASE_URL":"https://gemini-relay.test","GEMINI_API_KEY":"dk","GEMINI_MODEL":"gemini-2.5-flash"}`))
	request, err := deeplink.ParseDeepLinkURL("ccswitch://v1/import?resource=provider&app=gemini&name=DeepGemini&config=" + config)
	if err != nil {
		t.Fatal(err)
	}
	id, err := deeplink.ImportProviderFromDeepLink(request)
	if err != nil {
		t.Fatal(err)
	}
	providers := gemini.GetProviders()
	if len(providers) != 1 || providers[0].ID != id || providers[0].BaseURL != "https://gemini-relay.test" ||
		providers[0].APIKey != "dk" || providers[0].Model != "gemini-2.5-flash" || providers[0].Enabled ||
		providers[0].WebsiteURL != "https://gemini-relay.test" {
		t.Fatalf("unexpected deeplink Gemini provider: %+v", providers)
	}
}
