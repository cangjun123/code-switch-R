package services

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const newAPIKeyFixture = `{"code":true,"data":{"object":"token_usage","name":"test-key","total_granted":1000,"total_used":250,"total_available":750,"unlimited_quota":false,"expires_at":-1,"model_limits_enabled":true,"model_limits":{"allowed":true,"denied":false},"usedUSD":999,"secret":"do-not-forward"}}`
const newAPISiteFixture = `{"success":true,"data":{"quota_per_unit":100,"secret":"do-not-forward"}}`
const newAPIPriceFixture = `{"success":true,"data":[{"model_name":"test-model","quota_type":0,"model_ratio":2,"completion_ratio":3,"cache_ratio":0.1,"create_cache_ratio":1.25,"enable_groups":["standard","discount"]},{"model_name":"request-model","quota_type":1,"model_price":0.25,"enable_groups":["standard"]}],"group_ratio":{"standard":1.5,"discount":0.5}}`

func newAPIDraft(base string) ProviderInfoDraft {
	d := infoDraft(base)
	d.UpstreamInfo.Type = "newapi"
	return d
}
func serveNewAPI(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch r.URL.Path {
	case "/api/usage/token/":
		if r.Header.Get("Authorization") != "Bearer test-secret" && r.Header.Get("Authorization") != "Bearer changed-key" {
			t.Error("missing key on token endpoint")
		}
		fmt.Fprint(w, newAPIKeyFixture)
	case "/api/status", "/api/pricing":
		if r.Header.Get("Authorization") != "" {
			t.Error("key leaked to public endpoint")
		}
		if r.URL.Path == "/api/status" {
			fmt.Fprint(w, newAPISiteFixture)
		} else {
			fmt.Fprint(w, newAPIPriceFixture)
		}
	default:
		t.Errorf("unexpected path %s", r.URL.Path)
		http.NotFound(w, r)
	}
}
func TestProviderInfoNewAPIQuotaAndPrices(t *testing.T) {
	server := infoServer(t, func(w http.ResponseWriter, r *http.Request) { serveNewAPI(t, w, r) })
	got, err := NewProviderInfoService(nil, nil).TestConnection(newAPIDraft(server.URL), "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if got.Platform != "newapi" || got.UsageState.Status != "ready" || got.SiteState.Status != "ready" || got.PricingState.Status != "ready" {
		t.Fatalf("%+v", got)
	}
	if *got.Key.TotalUSD != 10 || *got.Key.UsedUSD != 2.5 || *got.Key.RemainingUSD != 7.5 || *got.Key.Remaining != 750 || *got.Key.ExpiresAt != -1 || !got.Key.ModelLimits["allowed"] {
		t.Fatalf("quota %+v", got.Key)
	}
	rows := got.Pricing.Rows
	if len(rows) != 3 || *rows[0].Request != 0.375 || *rows[1].Input != 2 || *rows[2].Input != 6 || *rows[2].Output != 18 || *rows[2].CacheRead != 0.6000000000000001 || *rows[2].CacheWrite != 7.5 {
		t.Fatalf("prices %+v", rows)
	}
	body, _ := json.Marshal(got)
	if strings.Contains(string(body), "do-not-forward") || got.Usage != nil || got.Billing != nil {
		t.Fatal("unexpected fields exposed")
	}
}
func TestProviderInfoNewAPIQuotaEdgeCases(t *testing.T) {
	for _, tc := range []struct {
		name, fields    string
		unlimited       bool
		used, remaining *float64
	}{
		{"missing", `"unlimited_quota":false`, false, nil, nil},
		{"zero", `"total_used":0,"total_available":0`, false, new(float64), new(float64)},
		{"negative", `"total_used":12,"total_available":-2`, false, ptrFloat(12), ptrFloat(-2)},
		{"unlimited", `"unlimited_quota":true,"total_used":12,"total_available":0`, true, ptrFloat(12), ptrFloat(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, status := decodeNewAPI([]byte(`{"code":true,"data":{"object":"token_usage",`+tc.fields+`}}`), "newapi_key")
			if status != "ready" {
				t.Fatal(status)
			}
			var key NewAPIKey
			_ = json.Unmarshal(raw, &key)
			if (key.Unlimited != nil && *key.Unlimited) != tc.unlimited || !equalFloat(key.Used, tc.used) || !equalFloat(key.Remaining, tc.remaining) {
				t.Fatalf("%+v", key)
			}
		})
	}
}
func ptrFloat(v float64) *float64   { return &v }
func equalFloat(a, b *float64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
func TestProviderInfoNewAPIValidation(t *testing.T) {
	for _, tc := range []struct{ kind, body, status string }{
		{"key", `{"code":false,"message":"test-secret"}`, "upstream_error"},
		{"site", `{"success":false}`, "upstream_error"},
		{"pricing", `{"success":false}`, "upstream_error"},
		{"key", `{"success":true,"data":{"object":"token_usage","total_used":0}}`, "invalid_response"},
		{"key", `{"code":true,"data":{}}`, "invalid_response"},
		{"key", `{"code":true,"data":{"object":"token_usage","total_used":"0"}}`, "invalid_response"},
		{"key", `{"code":true,"data":null}`, "invalid_response"},
		{"site", `{"success":true,"data":{"quota_per_unit":0}}`, "invalid_response"},
		{"site", `{"success":true,"data":{"quota_per_unit":-1}}`, "invalid_response"},
		{"site", `{"success":true,"data":{"quota_per_unit":null}}`, "invalid_response"},
		{"site", `{"success":true,"data":{"quota_per_unit":1e999}}`, "invalid_response"},
		{"pricing", `{"success":true,"data":null}`, "invalid_response"},
		{"pricing", `{"success":true,"data":[{}]}`, "invalid_response"},
		{"pricing", `<html>login</html>`, "invalid_response"},
		{"pricing", `{"success":true,"data":[]}`, "ready"},
	} {
		t.Run(tc.kind+tc.body, func(t *testing.T) {
			_, status := decodeNewAPI([]byte(tc.body), "newapi_"+tc.kind)
			if status != tc.status {
				t.Fatalf("got %s", status)
			}
		})
	}
}
func TestProviderInfoNewAPIPriceRules(t *testing.T) {
	for _, tc := range []struct {
		name, fields, groups, mode string
		input, output, request     *float64
		count                      int
	}{
		{"missing ratios", `"quota_type":0`, `{"g":1}`, "tokens", nil, nil, nil, 1},
		{"free", `"quota_type":0,"model_ratio":0,"completion_ratio":2`, `{"g":1}`, "tokens", ptrFloat(0), ptrFloat(0), nil, 1},
		{"missing completion", `"quota_type":0,"model_ratio":1`, `{"g":1}`, "tokens", ptrFloat(2), nil, nil, 1},
		{"missing group", `"quota_type":0,"model_ratio":1,"completion_ratio":2`, `{}`, "tokens", nil, nil, nil, 1},
		{"zero group", `"quota_type":0,"model_ratio":1,"completion_ratio":2`, `{"g":0}`, "tokens", ptrFloat(0), ptrFloat(0), nil, 1},
		{"negative ratio", `"quota_type":0,"model_ratio":-1`, `{"g":1}`, "tokens", nil, nil, nil, 1},
		{"per request free", `"quota_type":1,"model_price":0`, `{"g":1}`, "request", nil, nil, ptrFloat(0), 1},
		{"per request missing", `"quota_type":1`, `{"g":1}`, "request", nil, nil, nil, 1},
		{"expression", `"quota_type":0,"model_ratio":1,"billing_expr":"process.exit()"`, `{"g":1}`, "complex", nil, nil, nil, 1},
		{"tiered", `"quota_type":0,"model_ratio":1,"billing_mode":"tiered_expr"`, `{"g":1}`, "complex", nil, nil, nil, 1},
		{"plugin", `"quota_type":0,"model_ratio":1,"billing_plugin_variants":[{}]`, `{"g":1}`, "complex", nil, nil, nil, 1},
		{"audio", `"quota_type":0,"model_ratio":1,"audio_ratio":2`, `{"g":1}`, "complex", nil, nil, nil, 1},
		{"unknown type", `"quota_type":9,"model_ratio":1`, `{"g":1}`, "complex", nil, nil, nil, 1},
		{"missing type", `"model_ratio":1`, `{"g":1}`, "complex", nil, nil, nil, 1},
		{"overflow", `"quota_type":0,"model_ratio":1e308`, `{"g":10}`, "tokens", nil, nil, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"success":true,"data":[{"model_name":"model","enable_groups":["g","g"],` + tc.fields + `}],"group_ratio":` + tc.groups + `}`
			raw, status := decodeNewAPI([]byte(body), "newapi_pricing")
			if status != "ready" {
				t.Fatal(status)
			}
			var prices NewAPIPricing
			_ = json.Unmarshal(raw, &prices)
			if len(prices.Rows) != tc.count {
				t.Fatal("wrong group expansion")
			}
			row := prices.Rows[0]
			if row.Mode != tc.mode || !equalFloat(row.Input, tc.input) || !equalFloat(row.Output, tc.output) || !equalFloat(row.Request, tc.request) {
				t.Fatalf("%+v", row)
			}
		})
	}
	raw, status := decodeNewAPI([]byte(`{"success":true,"data":[{"model_name":"all","quota_type":0,"model_ratio":1,"enable_groups":["all"]}],"group_ratio":{"a":1,"b":2}}`), "newapi_pricing")
	var prices NewAPIPricing
	_ = json.Unmarshal(raw, &prices)
	if status != "ready" || len(prices.Rows) != 2 || prices.Rows[0].Group != "a" || *prices.Rows[1].Input != 4 {
		t.Fatal("all group expansion")
	}
}
func TestProviderInfoNewAPICacheAndPartial(t *testing.T) {
	var siteStatus, pricingStatus atomic.Int32
	siteStatus.Store(200)
	pricingStatus.Store(200)
	var quotaCalls, siteCalls, priceCalls atomic.Int32
	server := infoServer(t, func(w http.ResponseWriter, r *http.Request) {
		var status int32 = 200
		switch r.URL.Path {
		case "/api/usage/token/":
			quotaCalls.Add(1)
		case "/api/status":
			siteCalls.Add(1)
			status = siteStatus.Load()
		case "/api/pricing":
			priceCalls.Add(1)
			status = pricingStatus.Load()
		default:
			if strings.HasSuffix(r.URL.Path, "billing") {
				fmt.Fprint(w, infoBillingFixture)
			} else {
				fmt.Fprint(w, infoWalletFixture)
			}
			return
		}
		if status != 200 {
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(int(status))
			return
		}
		serveNewAPI(t, w, r)
	})
	svc := NewProviderInfoService(nil, nil)
	draft := newAPIDraft(server.URL)
	query := func(force bool) *ProviderInfoResult {
		got, err := svc.query(draft, "test", force, "UTC")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	query(false)
	query(false)
	if quotaCalls.Load() != 1 || siteCalls.Load() != 1 || priceCalls.Load() != 1 {
		t.Fatal("cache missed")
	}
	// Expire only the five-minute sections; prices should retain their longer TTL.
	svc.mu.Lock()
	for k, v := range svc.cache {
		if strings.HasSuffix(k, ":pricing") {
			if time.Until(v.expires) < 29*time.Minute {
				t.Error("price TTL too short")
			}
		} else {
			v.expires = time.Now().Add(-time.Second)
			svc.cache[k] = v
		}
	}
	svc.mu.Unlock()
	query(false)
	if quotaCalls.Load() != 2 || siteCalls.Load() != 2 || priceCalls.Load() != 1 {
		t.Fatal("independent TTLs")
	}
	siteStatus.Store(503)
	pricingStatus.Store(429)
	got := query(true)
	if !got.SiteState.Stale || !got.PricingState.Stale || got.UsageState.Stale || *got.Key.RemainingUSD != 7.5 || got.PricingState.RetryAt == "" {
		t.Fatalf("%+v", got)
	}
	query(true)
	if priceCalls.Load() != 2 {
		t.Fatal("manual refresh ignored Retry-After")
	}
	draft.APIKey = "changed-key"
	// Fail all public requests to ensure the new identity cannot see old cached data.
	got = query(false)
	if got.Site != nil || got.Key.RemainingUSD != nil || got.Pricing != nil {
		t.Fatal("key change retained private cache")
	}
	draft.APIKey = "test-secret"
	draft.UpstreamInfo.Type = "sub2api"
	got = query(false)
	if got.Platform != "sub2api" || got.Usage == nil || got.Key != nil {
		t.Fatal("platform cache collision")
	}
	draft.UpstreamInfo.Type = "newapi"
	draft.UpstreamInfo.BaseURL = server.URL + "/other"
	got = query(false)
	if got.Key != nil || got.Site != nil {
		t.Fatal("URL change retained cache")
	}
}
