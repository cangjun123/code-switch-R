package services

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

const deepSeekBalanceFixture = `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"12.34","granted_balance":"2.00","topped_up_balance":"10.34","secret":"must-not-forward"},{"currency":"USD","total_balance":"0.1234567890123456789","granted_balance":"0.00","topped_up_balance":"0.1234567890123456789"}],"secret":"must-not-forward"}`

func deepSeekDraft(base string) ProviderInfoDraft {
	return ProviderInfoDraft{APIURL: base, APIKey: "test-secret", UpstreamInfo: &UpstreamInfoConfig{Type: "deepseek"}}
}

func TestDeepSeekBalanceAndCache(t *testing.T) {
	var calls atomic.Int32
	server := infoServer(t, func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method != http.MethodGet || request.URL.Path != "/user/balance" || request.URL.RawQuery != "" {
			t.Errorf("unexpected balance endpoint: %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer test-secret" || request.Header.Get("Accept") != "application/json" {
			t.Error("missing balance request headers")
		}
		fmt.Fprint(w, deepSeekBalanceFixture)
	})
	service := NewProviderInfoService(nil, nil)
	draft := deepSeekDraft(server.URL + "/v1/")
	for _, force := range []bool{false, false, true} {
		result, err := service.query(draft, "test", force, "UTC")
		if err != nil {
			t.Fatal(err)
		}
		if result.Platform != "deepseek" || result.BalanceState == nil || result.BalanceState.Status != "ready" || result.BalanceState.UpdatedAt == "" || result.BalanceState.Stale {
			t.Fatalf("unexpected balance result: %+v", result)
		}
		if result.Balance == nil || !*result.Balance.IsAvailable || len(result.Balance.BalanceInfos) != 2 {
			t.Fatal("balance data missing")
		}
		if result.Balance.BalanceInfos[0].Currency != "CNY" || result.Balance.BalanceInfos[0].TotalBalance != "12.34" || result.Balance.BalanceInfos[1].TotalBalance != "0.1234567890123456789" {
			t.Fatal("upstream currency or decimal precision changed")
		}
		if result.Key != nil || result.Usage != nil || result.Billing != nil || result.Models != nil || result.UsageState.Status != "unsupported" || result.BillingState.Status != "unsupported" {
			t.Fatal("balance query must not imply independent key quota, usage or pricing")
		}
		body, _ := json.Marshal(result)
		if strings.Contains(string(body), "must-not-forward") || strings.Contains(string(body), "test-secret") {
			t.Fatal("unexpected upstream fields or credentials forwarded")
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("cache or forced refresh not respected: %d requests", calls.Load())
	}
	draft.APIURL = server.URL
	if _, err := service.TestConnection(draft, "UTC"); err != nil {
		t.Fatal(err)
	}
}

func TestDeepSeekServiceURL(t *testing.T) {
	server := infoServer(t, func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/service/user/balance" {
			t.Errorf("service path lost or /v1 incorrectly added: %s", request.URL.Path)
		}
		fmt.Fprint(w, deepSeekBalanceFixture)
	})
	for _, explicit := range []bool{false, true} {
		draft := deepSeekDraft(server.URL + "/service/v1/")
		if explicit {
			draft.APIURL = "https://unused.example/v1"
			draft.UpstreamInfo.BaseURL = server.URL + "/service/"
		}
		result, err := NewProviderInfoService(nil, nil).TestConnection(draft, "UTC")
		if err != nil || result.BalanceState.Status != "ready" {
			t.Fatalf("service URL query failed: %v", err)
		}
	}
}

func TestDecodeDeepSeekBalance(t *testing.T) {
	for _, fixture := range []string{
		`null`, `{}`, `{"is_available":true}`, `{"balance_infos":[]}`,
		`{"is_available":null,"balance_infos":[]}`, `{"is_available":false,"balance_infos":null}`,
		`{"is_available":false,"balance_infos":[null]}`,
		`{"is_available":false,"balance_infos":[{"currency":"CNY","total_balance":"0.00","granted_balance":"0.00"}]}`,
		strings.Replace(deepSeekBalanceFixture, `"total_balance":"12.34"`, `"total_balance":12.34`, 1),
		strings.Replace(deepSeekBalanceFixture, `"12.34"`, `"NaN"`, 1),
		strings.Replace(deepSeekBalanceFixture, `"12.34"`, `"1/3"`, 1),
		strings.Replace(deepSeekBalanceFixture, `"12.34"`, `"1e20"`, 1),
		strings.Replace(deepSeekBalanceFixture, `"CNY"`, `""`, 1),
		strings.Replace(deepSeekBalanceFixture, `"USD"`, `"CNY"`, 1),
	} {
		if data, status := decodeProviderInfo([]byte(fixture), "deepseek_balance"); status != "invalid_response" || data != nil {
			t.Errorf("accepted invalid balance: %s", fixture)
		}
	}
	for _, fixture := range []string{
		`{"is_available":false,"balance_infos":[]}`,
		`{"is_available":false,"balance_infos":[{"currency":"CNY","total_balance":"0.00","granted_balance":"0.00","topped_up_balance":"0.00"}]}`,
		`{"is_available":false,"balance_infos":[{"currency":"USD","total_balance":"-0.01","granted_balance":"0.00","topped_up_balance":"-0.01"}]}`,
	} {
		if data, status := decodeProviderInfo([]byte(fixture), "deepseek_balance"); status != "ready" || string(data) != fixture {
			t.Errorf("zero, negative or empty balances changed: %s %s", status, data)
		}
	}
}

func TestDeepSeekErrorsStaleAndCredentialIsolation(t *testing.T) {
	for _, test := range []struct {
		code   int
		status string
	}{{401, "auth"}, {403, "auth"}, {404, "unsupported"}, {405, "unsupported"}, {429, "rate_limited"}, {500, "upstream_error"}, {200, "invalid_response"}} {
		t.Run(test.status+fmt.Sprint(test.code), func(t *testing.T) {
			var fail atomic.Bool
			var calls atomic.Int32
			server := infoServer(t, func(w http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if fail.Load() {
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(test.code)
					fmt.Fprint(w, `{"error":"must-not-forward"}`)
					return
				}
				fmt.Fprint(w, deepSeekBalanceFixture)
			})
			service := NewProviderInfoService(nil, nil)
			draft := deepSeekDraft(server.URL)
			first, err := service.TestConnection(draft, "UTC")
			if err != nil {
				t.Fatal(err)
			}
			fail.Store(true)
			result, err := service.TestConnection(draft, "UTC")
			if err != nil {
				t.Fatal(err)
			}
			if result.BalanceState.Status != test.status || !result.BalanceState.Stale || result.BalanceState.UpdatedAt != first.BalanceState.UpdatedAt || result.Balance == nil {
				t.Fatalf("stale balance not identified: %+v", result)
			}
			body, _ := json.Marshal(result)
			if strings.Contains(string(body), "must-not-forward") {
				t.Fatal("raw upstream error forwarded")
			}
			if test.code == 429 {
				if result.BalanceState.RetryAt == "" {
					t.Fatal("retry time missing")
				}
				if _, err := service.TestConnection(draft, "UTC"); err != nil || calls.Load() != 2 {
					t.Fatal("forced refresh ignored retry backoff")
				}
			}
			draft.APIKey = "different-secret"
			result, err = service.TestConnection(draft, "UTC")
			if err != nil || result.Balance != nil || result.BalanceState.Stale || calls.Load() != 3 {
				t.Fatal("old account balance leaked across credentials")
			}
		})
	}
}

func TestDeepSeekRedirectAndBodyLimit(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { calls.Add(1) }))
	defer target.Close()
	redirect := infoServer(t, func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, target.URL, http.StatusFound)
	})
	result, err := NewProviderInfoService(nil, nil).TestConnection(deepSeekDraft(redirect.URL), "UTC")
	if err != nil || result.BalanceState.Status != "upstream_error" || calls.Load() != 0 {
		t.Fatal("balance requests must not follow redirects with credentials")
	}
	large := infoServer(t, func(w http.ResponseWriter, request *http.Request) { fmt.Fprint(w, strings.Repeat(" ", (2<<20)+1)) })
	result, err = NewProviderInfoService(nil, nil).TestConnection(deepSeekDraft(large.URL), "UTC")
	if err != nil || result.BalanceState.Status != "invalid_response" || result.Balance != nil {
		t.Fatal("oversized balance response accepted")
	}
}

func TestDeepSeekLiveBalance(t *testing.T) {
	base, key := os.Getenv("CODESWITCH_TEST_DEEPSEEK_URL"), os.Getenv("CODESWITCH_TEST_DEEPSEEK_KEY")
	if base == "" || key == "" {
		t.Skip("set CODESWITCH_TEST_DEEPSEEK_URL and CODESWITCH_TEST_DEEPSEEK_KEY for read-only live testing")
	}
	draft := deepSeekDraft(base)
	draft.APIKey = key
	result, err := NewProviderInfoService(nil, nil).TestConnection(draft, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if result.BalanceState.Status != "ready" || result.Balance == nil {
		t.Fatalf("balance query status: %s", result.BalanceState.Status)
	}
	t.Logf("queried balance for %d currencies", len(result.Balance.BalanceInfos))
}
