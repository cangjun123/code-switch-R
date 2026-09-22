package services

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

const accountFixture = `{"success":true,"data":{"quota":1234,"quotaUSD":999,"email":"private@example.com","access_token":"must-not-forward"}}`

func TestProviderInfoAccountAuthFallbackAndIsolation(t *testing.T) {
	var calls atomic.Int32
	var status atomic.Int32
	status.Store(200)
	server := infoServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user/self" {
			if r.Header.Get("New-Api-User") != "" || r.Header.Get("Authorization") == "Bearer account-secret" {
				t.Error("account credential leaked outside account endpoint")
			}
			serveNewAPI(t, w, r)
			return
		}
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer account-secret" || r.Header.Get("New-Api-User") != "42" {
			t.Error("account authentication headers incorrect")
		}
		if status.Load() != 200 {
			w.Header().Set("Retry-After", "300")
			w.WriteHeader(int(status.Load()))
			return
		}
		fmt.Fprint(w, accountFixture)
	})
	svc := NewProviderInfoService(nil, nil)
	d := newAPIDraft(server.URL)
	query := func(force bool) *ProviderInfoResult {
		got, err := svc.query(d, "codex:1", force, "UTC")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := query(false)
	if calls.Load() != 0 || got.Account != nil || got.AccountState != nil || got.Key == nil {
		t.Fatal("optional account queried without token")
	}
	d.UpstreamInfo.AccountToken = "account-secret"
	d.UpstreamInfo.AccountUserID = "42"
	got = query(false)
	if got.AccountState.Status != "ready" || *got.Account.Quota != 1234 || *got.Account.QuotaUSD != 12.34 || got.Key == nil {
		t.Fatalf("unexpected account result %+v", got)
	}
	encoded, _ := json.Marshal(got)
	for _, secret := range []string{"account-secret", "test-secret", "must-not-forward", "private@example.com"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("unexpected account field exposed")
		}
	}
	query(false)
	if calls.Load() != 1 {
		t.Fatal("account cache missed")
	}
	status.Store(401)
	got = query(true)
	if got.Account != nil || got.AccountState.Status != "auth" || got.AccountState.Stale || got.Key == nil {
		t.Fatal("invalid account did not fall back to key quota")
	}
	status.Store(200)
	got = query(true)
	if got.Account == nil {
		t.Fatal("account did not recover")
	}
	status.Store(429)
	got = query(true)
	query(true)
	if got.Account != nil || got.AccountState.Status != "rate_limited" || got.AccountState.RetryAt == "" || calls.Load() != 4 {
		t.Fatal("account Retry-After or fallback failed")
	}
	d.UpstreamInfo.AccountToken = ""
	got = query(false)
	if got.Account != nil || got.AccountState != nil || got.Key == nil || calls.Load() != 4 {
		t.Fatal("removing token retained account")
	}
}

func TestProviderInfoAccountIdentityAndFailureStates(t *testing.T) {
	var mode atomic.Int32
	var calls atomic.Int32
	server := infoServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user/self" {
			serveNewAPI(t, w, r)
			return
		}
		calls.Add(1)
		switch mode.Load() {
		case 0:
			fmt.Fprint(w, accountFixture)
		case 1:
			w.WriteHeader(401)
			fmt.Fprint(w, `{"success":false,"message":"Unauthorized, New-Api-User header not provided"}`)
		case 2:
			fmt.Fprint(w, `{"success":false,"message":"invalid account-secret"}`)
		case 3:
			fmt.Fprint(w, `{"success":true,"data":{"id":42}}`)
		case 4:
			fmt.Fprint(w, `{"success":true,"data":{"quota":0}}`)
		case 5:
			fmt.Fprint(w, `{"success":true,"data":{"quota":-100}}`)
		case 6:
			fmt.Fprint(w, `{"success":false,"message":"缺少 New-Api-User 请求头"}`)
		}
	})
	svc := NewProviderInfoService(nil, nil)
	d := newAPIDraft(server.URL)
	d.UpstreamInfo.AccountToken = "account-secret"
	got, _ := svc.query(d, "test", false, "UTC")
	if got.Account == nil {
		t.Fatal("site without user ID requirement failed")
	}
	mode.Store(1)
	d.UpstreamInfo.AccountToken = "new-account-secret"
	got, _ = svc.query(d, "test", false, "UTC")
	if got.Account != nil || got.AccountState.Status != "requires_user_id" {
		t.Fatal("credential change leaked previous account or missing user ID not identified")
	}
	mode.Store(0)
	d.UpstreamInfo.AccountUserID = "42"
	got, _ = svc.query(d, "test", false, "UTC")
	if got.Account == nil {
		t.Fatal("user ID change failed to clear error cache")
	}
	mode.Store(1)
	d.UpstreamInfo.AccountUserID = "43"
	got, _ = svc.query(d, "test", false, "UTC")
	if got.Account != nil || got.AccountState.Status != "auth" {
		t.Fatal("different user ID shared account cache")
	}
	before := calls.Load()
	d.UpstreamInfo.AccountUserID = "invalid"
	got, _ = svc.query(d, "test", false, "UTC")
	if got.Account != nil || got.AccountState.Status != "invalid_user_id" || calls.Load() != before || got.Key == nil {
		t.Fatal("invalid optional user ID blocked base queries")
	}
	d.UpstreamInfo.AccountUserID = ""
	d.UpstreamInfo.AccountToken = "bad\r\nheader"
	got, _ = svc.query(d, "test", false, "UTC")
	if got.Account != nil || got.AccountState.Status != "auth" || calls.Load() != before || got.Key == nil {
		t.Fatal("invalid optional token blocked base queries")
	}
	d.UpstreamInfo.AccountToken = "account-secret"
	for _, tc := range []struct {
		mode   int32
		status string
	}{{2, "upstream_error"}, {3, "invalid_response"}, {4, "ready"}, {5, "ready"}, {6, "requires_user_id"}} {
		mode.Store(tc.mode)
		got, _ = svc.query(d, "test", true, "UTC")
		if got.AccountState.Status != tc.status || got.Key == nil {
			t.Fatalf("mode %d: %+v", tc.mode, got)
		}
		if tc.mode == 4 && (*got.Account.Quota != 0 || *got.Account.QuotaUSD != 0) {
			t.Fatal("zero account balance lost")
		}
		if tc.mode == 5 && (*got.Account.Quota != -100 || *got.Account.QuotaUSD != -1) {
			t.Fatal("negative account balance lost")
		}
	}
	// Account lookup may be tested before a model API key is entered.
	mode.Store(0)
	d.APIKey = ""
	got, err := svc.TestConnection(d, "UTC")
	if err != nil || got.Account == nil || got.UsageState.Status != "missing_key" || got.Key != nil {
		t.Fatal("account-only preview failed")
	}
}

func TestProviderInfoAccountConversionAndRedirect(t *testing.T) {
	var siteOK atomic.Bool
	server := infoServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/user/self" {
			fmt.Fprint(w, accountFixture)
			return
		}
		if r.URL.Path == "/api/status" && !siteOK.Load() {
			w.WriteHeader(503)
			return
		}
		serveNewAPI(t, w, r)
	})
	svc := NewProviderInfoService(nil, nil)
	d := newAPIDraft(server.URL)
	d.UpstreamInfo.AccountToken = "account-secret"
	got, _ := svc.TestConnection(d, "UTC")
	if got.Account == nil || *got.Account.Quota != 1234 || got.Account.QuotaUSD != nil {
		t.Fatal("invalid conversion must retain raw account quota")
	}
	siteOK.Store(true)
	got, _ = svc.TestConnection(d, "UTC")
	if got.Account.QuotaUSD == nil {
		t.Fatal("account conversion did not recover")
	}
	siteOK.Store(false)
	got, _ = svc.TestConnection(d, "UTC")
	if !got.SiteState.Stale || *got.Account.QuotaUSD != 12.34 {
		t.Fatal("stale conversion must be marked")
	}
	var redirected atomic.Int32
	target := infoServer(t, func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) })
	redirect := infoServer(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) })
	d.UpstreamInfo.BaseURL = redirect.URL
	got, _ = svc.TestConnection(d, "UTC")
	if got.Account != nil || got.AccountState.Status != "upstream_error" || redirected.Load() != 0 {
		t.Fatal("account credential followed redirect or URL change retained account")
	}
}

// Opt-in, read-only test; no real credentials or account metadata are committed.
func TestProviderInfoNewAPIAccountLive(t *testing.T) {
	base, token := os.Getenv("CODESWITCH_TEST_NEWAPI_URL"), os.Getenv("CODESWITCH_TEST_NEWAPI_ACCOUNT_TOKEN")
	if base == "" || token == "" {
		t.Skip("set CODESWITCH_TEST_NEWAPI_URL and CODESWITCH_TEST_NEWAPI_ACCOUNT_TOKEN")
	}
	draft := newAPIDraft(base)
	draft.APIKey = ""
	draft.UpstreamInfo.AccountToken = token
	draft.UpstreamInfo.AccountUserID = os.Getenv("CODESWITCH_TEST_NEWAPI_USER_ID")
	got, err := NewProviderInfoService(nil, nil).TestConnection(draft, "UTC")
	if err != nil {
		t.Fatal("account query failed")
	}
	if got.AccountState.Status != "ready" || got.Account == nil || got.Account.Quota == nil {
		t.Fatalf("account=%s", got.AccountState.Status)
	}
	if got.SiteState.Status != "ready" || got.Account.QuotaUSD == nil {
		t.Fatalf("site=%s", got.SiteState.Status)
	}
	t.Logf("read-only wallet query and USD conversion succeeded; public pricing=%s", got.PricingState.Status)
}
