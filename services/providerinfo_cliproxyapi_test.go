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

const cliProxyModelsFixture = `{"object":"list","data":[{"id":"model-b","owned_by":"openai","secret":"must-not-forward"},{"id":"model-a","owned_by":"google"},{"id":"model-b"}],"secret":"must-not-forward"}`

func cliProxyDraft(base string) ProviderInfoDraft {
	return ProviderInfoDraft{APIURL: base + "/v1", APIKey: "test-secret", UpstreamInfo: &UpstreamInfoConfig{Type: "cliproxyapi"}}
}

func TestCLIProxyAPIModels(t *testing.T) {
	var calls atomic.Int32
	server := infoServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/proxy/v1/models" || r.URL.RawQuery != "" {
			t.Errorf("unexpected endpoint: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("API credential missing")
		}
		fmt.Fprint(w, cliProxyModelsFixture)
	})
	service := NewProviderInfoService(nil, nil)
	draft := cliProxyDraft(server.URL + "/proxy")
	for _, force := range []bool{false, false, true} {
		result, err := service.query(draft, "test", force, "UTC")
		if err != nil {
			t.Fatal(err)
		}
		if result.Platform != "cliproxyapi" || result.ModelsState == nil || result.ModelsState.Status != "ready" || result.ModelsState.UpdatedAt == "" || result.ModelsState.Stale {
			t.Fatalf("unexpected result: %+v", result)
		}
		if result.Models == nil || len(result.Models.Data) != 2 || result.Models.Data[0].ID != "model-a" || result.Models.Data[1].OwnedBy != "openai" {
			t.Fatalf("unexpected models: %+v", result.Models)
		}
		if result.Usage != nil || result.Billing != nil || result.Key != nil || result.UsageState.Status != "unsupported" || result.BillingState.Status != "unsupported" {
			t.Fatal("model queries must not imply quota or billing support")
		}
		body, _ := json.Marshal(result)
		if strings.Contains(string(body), "must-not-forward") || strings.Contains(string(body), "test-secret") {
			t.Fatal("unexpected upstream fields or credentials forwarded")
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("cache not respected: %d requests", calls.Load())
	}
	if _, err := service.TestConnection(draft, "UTC"); err != nil {
		t.Fatal(err)
	}
	draft.UpstreamInfo.BaseURL = server.URL + "/proxy/"
	if _, err := service.TestConnection(draft, "UTC"); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeCLIProxyModels(t *testing.T) {
	for _, fixture := range []string{`null`, `{}`, `{"data":null}`, `{"data":{}}`, `{"data":[null]}`, `{"data":[{"id":" "}]}`, `{"data":[{"id":123}]}`, `{"error":"secret"}`} {
		if data, status := decodeProviderInfo([]byte(fixture), "cliproxyapi_models"); status != "invalid_response" || data != nil {
			t.Errorf("accepted invalid model list: %s", fixture)
		}
	}
	if data, status := decodeProviderInfo([]byte(`{"data":[]}`), "cliproxyapi_models"); status != "ready" || string(data) != `{"data":[]}` {
		t.Fatalf("empty model list must remain valid: %s %s", status, data)
	}
}

func TestCLIProxyAPIErrorsAndStaleModels(t *testing.T) {
	for _, test := range []struct {
		code   int
		status string
	}{{401, "auth"}, {403, "auth"}, {404, "unsupported"}, {405, "unsupported"}, {429, "rate_limited"}, {500, "upstream_error"}, {200, "invalid_response"}} {
		t.Run(test.status+fmt.Sprint(test.code), func(t *testing.T) {
			var fail atomic.Bool
			var calls atomic.Int32
			server := infoServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer test-secret" && r.Header.Get("Authorization") != "Bearer different-secret" {
					t.Error("wrong API credential")
				}
				if fail.Load() {
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(test.code)
					fmt.Fprint(w, `{"error":"must-not-forward"}`)
					return
				}
				fmt.Fprint(w, cliProxyModelsFixture)
			})
			service := NewProviderInfoService(nil, nil)
			draft := cliProxyDraft(server.URL)
			first, err := service.query(draft, "test", true, "UTC")
			if err != nil {
				t.Fatal(err)
			}
			fail.Store(true)
			result, err := service.query(draft, "test", true, "UTC")
			if err != nil {
				t.Fatal(err)
			}
			if result.ModelsState.Status != test.status || !result.ModelsState.Stale || result.ModelsState.UpdatedAt != first.ModelsState.UpdatedAt || result.Models == nil {
				t.Fatalf("stale models lost: %+v", result)
			}
			if test.code == 429 {
				if result.ModelsState.RetryAt == "" {
					t.Fatal("retry time missing")
				}
				if _, err := service.query(draft, "test", true, "UTC"); err != nil || calls.Load() != 2 {
					t.Fatal("forced refresh ignored retry backoff")
				}
			}
			draft.APIKey = "different-secret"
			result, err = service.query(draft, "test", false, "UTC")
			if err != nil || result.Models != nil || result.ModelsState.Stale || calls.Load() != 3 {
				t.Fatal("stale model list leaked across credentials")
			}
		})
	}
}

func TestCLIProxyAPIRedirect(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer target.Close()
	server := infoServer(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) })
	result, err := NewProviderInfoService(nil, nil).TestConnection(cliProxyDraft(server.URL), "UTC")
	if err != nil || result.ModelsState.Status != "upstream_error" || calls.Load() != 0 {
		t.Fatal("model queries must not follow redirects with credentials")
	}
}

func TestCLIProxyAPILive(t *testing.T) {
	base, key := os.Getenv("CODESWITCH_TEST_CLIPROXYAPI_URL"), os.Getenv("CODESWITCH_TEST_CLIPROXYAPI_KEY")
	if base == "" || key == "" {
		t.Skip("set CODESWITCH_TEST_CLIPROXYAPI_URL and CODESWITCH_TEST_CLIPROXYAPI_KEY for read-only live testing")
	}
	draft := cliProxyDraft("")
	draft.APIURL, draft.APIKey = base, key
	result, err := NewProviderInfoService(nil, nil).TestConnection(draft, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if result.ModelsState.Status != "ready" || result.Models == nil {
		t.Fatalf("model query status: %s", result.ModelsState.Status)
	}
	t.Logf("queried %d upstream models", len(result.Models.Data))
}
