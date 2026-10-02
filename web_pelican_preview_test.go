package main

import (
	"codeswitch/services"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPelicanPreviewAuthenticationAndSecurityHeaders(t *testing.T) {
	rt := newTestWebRuntime(t)
	html := `<!DOCTYPE html><html><body><svg></svg><script>document.body.dataset.animated='true'</script></body></html>`
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": html}, "finish_reason": "stop"}}})
	}))
	defer upstream.Close()
	if err := rt.providerService.SaveProviders("codex", []services.Provider{{ID: 42, Name: "preview", APIURL: upstream.URL, APIEndpoint: "/chat/completions", UpstreamProtocol: "auto"}}); err != nil {
		t.Fatal(err)
	}
	rt.pelicanTestService = services.NewPelicanTestService(rt.providerService)
	defer rt.pelicanTestService.Stop()
	server := newAdminServer(rt)
	initialized := performRequest(t, server.Handler, http.MethodPost, "/api/admin/initialize", map[string]string{"username": "admin", "password": "password123"})
	if initialized.Code != http.StatusOK {
		t.Fatalf("initialize: %s", initialized.Body.String())
	}
	cookie := initialized.Result().Cookies()[0]
	start := performRequest(t, server.Handler, http.MethodPost, "/api/wails/call", map[string]any{"name": "codeswitch/services.PelicanTestService.StartTest", "args": []any{"codex", 42, "test-model"}}, cookie)
	if start.Code != http.StatusOK {
		t.Fatalf("start: %s", start.Body.String())
	}
	var payload struct {
		Data services.PelicanTestResult `json:"data"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		result, err := rt.pelicanTestService.GetTest(payload.Data.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "running" {
			payload.Data = result
			break
		}
		time.Sleep(time.Millisecond)
	}
	if payload.Data.Status != "completed" {
		t.Fatalf("result = %+v", payload.Data)
	}
	path := "/api/pelican/preview/" + payload.Data.SessionID + "?token=" + payload.Data.PreviewToken
	unauthenticated := performRequest(t, server.Handler, http.MethodGet, path, nil)
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", unauthenticated.Code)
	}
	for _, invalidPath := range []string{"/api/pelican/preview/missing?token=token", "/api/pelican/preview/" + payload.Data.SessionID, "/api/pelican/preview/" + payload.Data.SessionID + "?token=wrong"} {
		response := performRequest(t, server.Handler, http.MethodGet, invalidPath, nil, cookie)
		if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), html) {
			t.Fatalf("invalid preview: %d", response.Code)
		}
	}
	preview := performRequest(t, server.Handler, http.MethodGet, path, nil, cookie)
	if preview.Code != http.StatusOK || preview.Body.String() != html {
		t.Fatalf("preview: %d %s", preview.Code, preview.Body.String())
	}
	if !strings.HasPrefix(preview.Header().Get("Content-Type"), "text/html") || preview.Header().Get("X-Frame-Options") != "" {
		t.Fatal("invalid embedding headers")
	}
	if preview.Header().Get("Content-Security-Policy") != pelicanPreviewCSP {
		t.Fatal("preview CSP missing")
	}
	for _, directive := range []string{"sandbox allow-scripts", "connect-src 'none'", "frame-ancestors 'self'", "form-action 'none'", "script-src 'unsafe-inline'", "default-src 'none'"} {
		if !strings.Contains(preview.Header().Get("Content-Security-Policy"), directive) {
			t.Errorf("missing %s", directive)
		}
	}
	if preview.Header().Get("Cache-Control") != "no-store" || preview.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("preview caching/referrer headers missing")
	}
	health := performRequest(t, server.Handler, http.MethodGet, "/healthz", nil)
	if !strings.Contains(health.Header().Get("Content-Security-Policy"), "script-src 'self'") || health.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("admin security headers changed")
	}
	if calls.Load() != 1 {
		t.Fatal("preview made another generation request")
	}
	get := performRequest(t, server.Handler, http.MethodPost, "/api/wails/call", map[string]any{"name": "codeswitch/services.PelicanTestService.GetTest", "args": []any{payload.Data.SessionID}}, cookie)
	if get.Code != http.StatusOK {
		t.Fatal("GetTest RPC unavailable")
	}
	data, _ := io.ReadAll(get.Result().Body)
	if !strings.Contains(string(data), payload.Data.SessionID) {
		t.Fatal("result recovery missing")
	}
}
