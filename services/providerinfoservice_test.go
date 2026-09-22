package services

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const infoWalletFixture = `{"mode":"unrestricted","isValid":true,"balance":12.3456,"remaining":12.3456,"unit":"USD","planName":"Wallet","usage":{"today":{"requests":3,"cost":10,"actual_cost":2},"total":{"requests":9,"actual_cost":7}},"daily_usage":[{"date":"2026-01-01","actual_cost":2,"cache_write_tokens":0}],"model_stats":[{"model":"test-model","actual_cost":2,"account_cost":10}],"secret":"must-not-forward"}`
const infoBillingFixture = `{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","effective_rate_multiplier":0,"group_rate_multiplier":0,"peak_rate_enabled":false}`

func infoDraft(base string) ProviderInfoDraft {
	return ProviderInfoDraft{APIURL: base + "/v1", APIKey: "test-secret", UpstreamInfo: &UpstreamInfoConfig{Type: "sub2api"}}
}
func infoServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}
func TestProviderInfoURL(t *testing.T) {
	for _, tt := range []struct{ base, override, want string }{
		{"https://example.com/v1/", "", "https://example.com"},
		{"https://example.com/prefix/v1beta", "", "https://example.com/prefix"},
		{"https://example.com/prefix", "", "https://example.com/prefix"},
		{"https://example.com/v1", "https://panel.example.com/prefix/", "https://panel.example.com/prefix"},
		{"https://example.com/v1", "https://panel.example.com/v1", "https://panel.example.com/v1"},
	} {
		draft := infoDraft(tt.base)
		draft.APIURL = tt.base
		draft.UpstreamInfo.BaseURL = tt.override
		got, err := providerInfoBase(draft)
		if err != nil || got != tt.want {
			t.Errorf("base(%s,%s)=%s,%v", tt.base, tt.override, got, err)
		}
	}
	for _, raw := range []string{"ftp://example.com", "https://user:secret@example.com", "https://example.com?key=secret", "https://example.com#x", "/relative", "http://"} {
		d := infoDraft(raw)
		d.APIURL = raw
		if _, err := providerInfoBase(d); err == nil {
			t.Errorf("accepted invalid URL %q", raw)
		}
	}
}
func TestProviderInfoWalletBillingAndCache(t *testing.T) {
	var calls atomic.Int32
	server := infoServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("missing bearer")
		}
		if strings.HasSuffix(r.URL.Path, "/billing") {
			fmt.Fprint(w, infoBillingFixture)
			return
		}
		if r.URL.Query().Get("timezone") != "Asia/Shanghai" || r.URL.Query().Get("days") != "30" {
			t.Error("missing range/timezone")
		}
		fmt.Fprint(w, infoWalletFixture)
	})
	svc := NewProviderInfoService(nil, nil)
	d := infoDraft(server.URL)
	result, err := svc.query(d, "codex:1", false, "Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	if result.UsageState.Status != "ready" || result.BillingState.Status != "ready" || result.Usage.Balance == nil || *result.Usage.Balance != 12.3456 || *result.Billing.EffectiveRate != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if *result.Usage.Usage.Today.ActualCost != 2 {
		t.Fatal("historical charge changed by current multiplier")
	}
	body, _ := json.Marshal(result)
	if strings.Contains(string(body), "test-secret") || strings.Contains(string(body), "must-not-forward") || strings.Contains(string(body), "account_cost") {
		t.Fatal("unexpected fields exposed")
	}
	if _, err := svc.query(d, "codex:1", false, "Asia/Shanghai"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("cache miss")
	}
	if _, err := svc.query(d, "codex:1", true, "Asia/Shanghai"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Fatal("force did not refresh")
	}
}
func TestProviderInfoModesAndMissingFields(t *testing.T) {
	fixtures := []string{
		`{"mode":"quota_limited","isValid":true,"status":"expired","quota":{"limit":20,"used":20,"remaining":0,"unit":"USD"},"rate_limits":[{"window":"5h","limit":10,"used":10,"remaining":0,"reset_at":"2026-01-01T01:00:00Z"}],"expires_at":"2026-01-01T00:00:00Z"}`,
		`{"mode":"unrestricted","isValid":true,"remaining":-1,"unit":"USD","subscription":{"daily_usage_usd":0,"daily_limit_usd":null,"expires_at":"2026-01-01T00:00:00Z"}}`,
		`{"mode":"quota_limited","isValid":true,"rate_limits":[{"window":"1d","remaining":2}]}`,
	}
	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			server := infoServer(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/billing") {
					http.NotFound(w, r)
					return
				}
				fmt.Fprint(w, fixture)
			})
			result, err := NewProviderInfoService(nil, nil).TestConnection(infoDraft(server.URL), "UTC")
			if err != nil {
				t.Fatal(err)
			}
			if result.Usage == nil || result.UsageState.Status != "ready" || result.BillingState.Status != "unsupported" {
				t.Fatal("partial data discarded")
			}
			if result.Usage.Balance != nil {
				t.Fatal("missing balance coerced to zero")
			}
			if result.Usage.Subscription != nil && result.Usage.Subscription.DailyLimit != nil {
				t.Fatal("null limit coerced to zero")
			}
		})
	}
}
func TestProviderInfoErrorsAndStaleIsolation(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	server := infoServer(t, func(w http.ResponseWriter, r *http.Request) {
		if code := int(status.Load()); code != 200 {
			w.WriteHeader(code)
			fmt.Fprint(w, "secret upstream body")
			return
		}
		if strings.HasSuffix(r.URL.Path, "/billing") {
			fmt.Fprint(w, infoBillingFixture)
		} else {
			fmt.Fprint(w, infoWalletFixture)
		}
	})
	svc := NewProviderInfoService(nil, nil)
	draft := infoDraft(server.URL)
	initial, _ := svc.query(draft, "codex:1", true, "UTC")
	status.Store(401)
	stale, _ := svc.query(draft, "codex:1", true, "UTC")
	if stale.UsageState.Status != "auth" || !stale.UsageState.Stale || stale.UsageState.UpdatedAt != initial.UsageState.UpdatedAt || stale.Usage == nil {
		t.Fatal("last good data not retained")
	}
	for _, change := range []string{"key", "ref", "url", "timezone"} {
		d := draft
		ref := "codex:1"
		tz := "UTC"
		switch change {
		case "key":
			d.APIKey = "other-secret"
		case "ref":
			ref = "claude:1"
		case "url":
			d.APIURL = server.URL + "/prefix"
		case "timezone":
			tz = "Asia/Shanghai"
		}
		got, err := svc.query(d, ref, true, tz)
		if err != nil {
			t.Fatal(err)
		}
		if got.Usage != nil || got.UsageState.Stale {
			t.Errorf("old data exposed after %s changed", change)
		}
	}
}
func TestProviderInfoBadResponses(t *testing.T) {
	for _, tt := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"HTML", 200, "<html>login</html>", "invalid_response"},
		{"JSON error", 200, `{"success":false,"message":"secret"}`, "invalid_response"},
		{"empty object", 200, `{}`, "invalid_response"},
		{"wrong type", 200, `{"mode":"unrestricted","isValid":true,"balance":"oops"}`, "invalid_response"},
		{"oversize", 200, strings.Repeat(" ", 2<<20) + infoWalletFixture, "invalid_response"},
		{"forbidden", 403, "secret", "auth"}, {"not found", 404, "secret", "unsupported"}, {"unavailable", 503, "secret", "upstream_error"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := infoServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tt.status); fmt.Fprint(w, tt.body) })
			got, err := NewProviderInfoService(nil, nil).TestConnection(infoDraft(server.URL), "UTC")
			if err != nil {
				t.Fatal(err)
			}
			if got.UsageState.Status != tt.want || got.Usage != nil {
				t.Fatalf("status=%s", got.UsageState.Status)
			}
			b, _ := json.Marshal(got)
			if strings.Contains(string(b), "secret") {
				t.Fatal("upstream body leaked")
			}
		})
	}
}
func TestProviderInfoRateLimitAndRedirect(t *testing.T) {
	var calls atomic.Int32
	server := infoServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
	})
	svc := NewProviderInfoService(nil, nil)
	draft := infoDraft(server.URL)
	for i := 0; i < 2; i++ {
		got, err := svc.TestConnection(draft, "UTC")
		if err != nil {
			t.Fatal(err)
		}
		if got.UsageState.Status != "rate_limited" || got.UsageState.RetryAt == "" {
			t.Fatal("missing backoff")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("force bypassed rate limit")
	}
	var redirected atomic.Int32
	target := infoServer(t, func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) })
	redirect := infoServer(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) })
	got, _ := svc.TestConnection(infoDraft(redirect.URL), "UTC")
	if redirected.Load() != 0 || got.UsageState.Status != "upstream_error" {
		t.Fatal("followed credential-bearing redirect")
	}
}
func TestProviderInfoTimeoutAndCoalescing(t *testing.T) {
	var calls, active, maxActive atomic.Int32
	server := infoServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for old := maxActive.Load(); n > old; old = maxActive.Load() {
			if maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case <-time.After(70 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		if strings.HasSuffix(r.URL.Path, "/billing") {
			fmt.Fprint(w, infoBillingFixture)
		} else {
			fmt.Fprint(w, infoWalletFixture)
		}
	})
	svc := NewProviderInfoService(nil, nil)
	draft := infoDraft(server.URL)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.TestConnection(draft, "UTC"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 2 {
		t.Errorf("overlapping refreshes made %d calls", calls.Load())
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, _ = svc.query(draft, fmt.Sprint(i), true, "UTC") }(i)
	}
	wg.Wait()
	if maxActive.Load() > 4 {
		t.Errorf("max concurrency %d", maxActive.Load())
	}
	svc.client.Timeout = time.Millisecond
	got, err := svc.query(draft, "timeout", true, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if got.UsageState.Status != "network" {
		t.Fatal("timeout not classified")
	}
}
func TestProviderInfoPersistenceAndReferences(t *testing.T) {
	for _, platform := range []string{"sub2api", "newapi"} {
		t.Run(platform, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			server := infoServer(t, func(w http.ResponseWriter, r *http.Request) {
				if platform == "newapi" {
					serveNewAPI(t, w, r)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/billing") {
					fmt.Fprint(w, infoBillingFixture)
				} else {
					fmt.Fprint(w, infoWalletFixture)
				}
			})
			providers := NewProviderService()
			gemini := NewGeminiService("")
			config := &UpstreamInfoConfig{Type: platform, BaseURL: server.URL}
			p := Provider{ID: 42, Name: "Info test", APIURL: server.URL + "/v1", APIKey: "test-secret", UpstreamInfo: config}
			for _, kind := range []string{"claude", "codex", "gpt-image", "custom:test"} {
				if err := providers.SaveProviders(kind, []Provider{p}); err != nil {
					t.Fatal(err)
				}
			}
			if err := gemini.AddProvider(GeminiProvider{ID: "native-id", Name: "Info Gemini", BaseURL: server.URL, APIKey: "test-secret", UpstreamInfo: config}); err != nil {
				t.Fatal(err)
			}
			reloaded := NewGeminiService("")
			svc := NewProviderInfoService(providers, reloaded)
			for _, ref := range []ProviderInfoRef{{"claude", "42"}, {"codex", "42"}, {"gpt-image", "42"}, {"custom:test", "42"}, {"gemini", "native-id"}} {
				got, err := svc.GetInfo(ref, false, "UTC")
				if err != nil || (platform == "sub2api" && got.Usage == nil) || (platform == "newapi" && got.Key == nil) {
					t.Fatalf("ref %+v: %v", ref, err)
				}
			}
			if _, err := svc.GetInfo(ProviderInfoRef{"gemini", "300"}, false, "UTC"); err == nil {
				t.Fatal("accepted synthetic Gemini id")
			}
			for _, copyKind := range []string{"codex", "custom:test"} {
				copy, err := providers.DuplicateProvider(copyKind, 42)
				if err != nil {
					t.Fatal(err)
				}
				if copy.UpstreamInfo == nil || copy.UpstreamInfo.BaseURL != server.URL || copy.UpstreamInfo.Type != platform {
					t.Fatal("copy lost info config")
				}
			}
			geminiCopy, err := reloaded.DuplicateProvider("native-id")
			if err != nil || geminiCopy.UpstreamInfo == nil || geminiCopy.UpstreamInfo.BaseURL != server.URL || geminiCopy.UpstreamInfo.Type != platform {
				t.Fatal("Gemini copy lost info config")
			}
			p.UpstreamInfo = nil
			if err := providers.SaveProviders("claude", []Provider{p}); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.GetInfo(ProviderInfoRef{"claude", "42"}, false, "UTC"); err == nil || err.Error() != "info_disabled" {
				t.Fatal("disabled config still queried")
			}
		})
	}
}

// Explicitly opt in to a read-only integration check. No site or credential is checked in.
func TestProviderInfoLive(t *testing.T) {
	base, key := os.Getenv("CODESWITCH_TEST_SUB2API_URL"), os.Getenv("CODESWITCH_TEST_SUB2API_KEY")
	if base == "" || key == "" {
		t.Skip("set CODESWITCH_TEST_SUB2API_URL and CODESWITCH_TEST_SUB2API_KEY to query a real upstream")
	}
	d := infoDraft(base)
	d.APIURL = base
	d.APIKey = key
	got, err := NewProviderInfoService(nil, nil).TestConnection(d, "Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	if got.UsageState.Status != "ready" || got.BillingState.Status != "ready" {
		t.Fatalf("usage=%s billing=%s", got.UsageState.Status, got.BillingState.Status)
	}
	t.Logf("read-only query succeeded: daily rows=%d, model rows=%d", len(got.Usage.DailyUsage), len(got.Usage.ModelStats))
}
