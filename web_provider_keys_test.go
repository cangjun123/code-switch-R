package main

import (
	"codeswitch/services"
	"encoding/json"
	"net/http"
	"testing"
)

func TestProviderKeyRoutesEnsureListAndPruneOnProviderDelete(t *testing.T) {
	rt := newTestWebRuntime(t)
	rt.providerService.SetProvidersSavedHook(func(kind string, providers []services.Provider) {
		pruneProviderRelayKeys(rt.codexRelayKeys, kind, providers)
	})
	if err := rt.providerService.SaveProviders("codex", []services.Provider{
		{ID: 1, Name: "Alpha", APIURL: "https://a.example", APIKey: "a"},
		{ID: 2, Name: "Beta", APIURL: "https://b.example", APIKey: "b"},
	}); err != nil {
		t.Fatal(err)
	}
	server := newAdminServer(rt)

	request := map[string]any{"kind": "codex", "providerId": 1}
	if response := performRequest(t, server.Handler, http.MethodPost, "/api/admin/provider-keys", request); response.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthenticated request to be rejected, got %d", response.Code)
	}
	initialize := performRequest(t, server.Handler, http.MethodPost, "/api/admin/initialize", map[string]string{"username": "admin", "password": "password123"})
	if initialize.Code != http.StatusOK {
		t.Fatalf("initialization failed: %d", initialize.Code)
	}
	cookie := initialize.Result().Cookies()[0]

	var first, second providerKeyEnsureResponse
	response := performRequest(t, server.Handler, http.MethodPost, "/api/admin/provider-keys", request, cookie)
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &first) != nil {
		t.Fatalf("ensure failed: %d %s", response.Code, response.Body.String())
	}
	if !first.Created || first.Key == "" || first.Name != "Codex · Alpha" || first.ProviderID != 1 {
		t.Fatalf("unexpected ensure response: %+v", first)
	}
	response = performRequest(t, server.Handler, http.MethodPost, "/api/admin/provider-keys", request, cookie)
	if json.Unmarshal(response.Body.Bytes(), &second) != nil || second.Created || second.ID != first.ID || second.Key != first.Key {
		t.Fatalf("expected existing key to be returned, got %+v", second)
	}

	missing := performRequest(t, server.Handler, http.MethodPost, "/api/admin/provider-keys", map[string]any{"kind": "codex", "providerId": 9}, cookie)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("expected missing provider to return 404, got %d", missing.Code)
	}
	gemini := performRequest(t, server.Handler, http.MethodPost, "/api/admin/provider-keys", map[string]any{"kind": "gemini", "providerId": 1}, cookie)
	if gemini.Code != http.StatusBadRequest {
		t.Fatalf("expected Gemini kind to be rejected, got %d", gemini.Code)
	}

	var listed struct {
		Keys []providerKeyItem `json:"keys"`
	}
	response = performRequest(t, server.Handler, http.MethodGet, "/api/admin/provider-keys?kind=codex", nil, cookie)
	if json.Unmarshal(response.Body.Bytes(), &listed) != nil || len(listed.Keys) != 1 || listed.Keys[0].ProviderID != 1 || listed.Keys[0].MaskedKey == first.Key {
		t.Fatalf("unexpected list response: %s", response.Body.String())
	}

	// Deleting the provider removes its dedicated key.
	if err := rt.providerService.SaveProviders("codex", []services.Provider{
		{ID: 2, Name: "Beta", APIURL: "https://b.example", APIKey: "b"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.codexRelayKeys.GetKeyByID(first.ID); err == nil {
		t.Fatal("expected dedicated key to be deleted with its provider")
	}
}
