package main

import (
	"codeswitch/services"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProviderInfoRPCRequiresAdminAndReturnsSanitizedData(t *testing.T) {
	rt := newTestWebRuntime(t)
	rt.providerInfoService = services.NewProviderInfoService(rt.providerService, nil)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer rpc-test-secret" {
			t.Error("query did not use persisted key")
		}
		if strings.HasSuffix(r.URL.Path, "/billing") {
			w.WriteHeader(404)
			return
		}
		fmt.Fprint(w, `{"mode":"unrestricted","isValid":true,"balance":5,"unit":"USD","apiKey":"do-not-forward"}`)
	}))
	defer upstream.Close()
	if err := rt.providerService.SaveProviders("codex", []services.Provider{{ID: 1, Name: "RPC test", APIURL: upstream.URL, APIKey: "rpc-test-secret", UpstreamInfo: &services.UpstreamInfoConfig{Type: "sub2api"}}}); err != nil {
		t.Fatal(err)
	}
	server := newAdminServer(rt)
	body := map[string]any{"name": "codeswitch/services.ProviderInfoService.GetInfo", "args": []any{map[string]string{"kind": "codex", "id": "1"}, false, "UTC"}}
	unauth := performRequest(t, server.Handler, http.MethodPost, "/api/wails/call", body)
	if unauth.Code != 401 || calls.Load() != 0 {
		t.Fatal("unauthenticated query reached upstream")
	}
	initialize := performRequest(t, server.Handler, http.MethodPost, "/api/admin/initialize", map[string]string{"username": "admin", "password": "password123"})
	if initialize.Code != 200 {
		t.Fatal("initialization failed")
	}
	cookie := initialize.Result().Cookies()[0]
	response := performRequest(t, server.Handler, http.MethodPost, "/api/wails/call", body, cookie)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"balance":5`) || calls.Load() != 2 {
		t.Fatalf("unexpected RPC response: %d", response.Code)
	}
	if strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "do-not-forward") {
		t.Fatal("credential or unapproved field leaked")
	}
}
