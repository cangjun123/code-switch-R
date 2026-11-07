package services

import (
	"encoding/json"
	"testing"
)

func TestEnsureProviderKeyIsIdempotentPerProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service := NewCodexRelayKeyService()

	first, created, err := service.EnsureProviderKey(ProviderKindCodex, 3, "Codex · A")
	if err != nil || !created {
		t.Fatalf("EnsureProviderKey(first) = created %v, err %v", created, err)
	}
	again, created, err := service.EnsureProviderKey(ProviderKindCodex, 3, "Codex · renamed")
	if err != nil || created {
		t.Fatalf("EnsureProviderKey(again) = created %v, err %v", created, err)
	}
	if again.ID != first.ID || again.Key != first.Key || again.Name != "Codex · A" {
		t.Fatalf("expected existing key to be reused, got %+v vs %+v", again, first)
	}
	// Provider IDs are per kind: the same ID under Claude is a different provider.
	claude, created, err := service.EnsureProviderKey(ProviderKindClaude, 3, "Claude · A")
	if err != nil || !created || claude.ID == first.ID {
		t.Fatalf("expected a separate Claude key, got %+v created %v err %v", claude, created, err)
	}
	if _, _, err := service.EnsureProviderKey(ProviderKindGemini, 1, "Gemini"); err == nil {
		t.Fatal("expected Gemini binding to be rejected")
	}

	listed, err := service.ListProviderKeys(ProviderKindCodex)
	if err != nil || len(listed) != 1 || listed[0].ProviderBinding.ProviderID != 3 {
		t.Fatalf("ListProviderKeys(codex) = %+v, err %v", listed, err)
	}
}

func TestProviderKeyIsNotUsedAsDefaultKeyAndCanBeDeleted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service := NewCodexRelayKeyService()

	bound, _, err := service.EnsureProviderKey(ProviderKindCodex, 1, "Codex · A")
	if err != nil {
		t.Fatalf("EnsureProviderKey failed: %v", err)
	}
	defaultKey, err := service.EnsureDefaultKey()
	if err != nil {
		t.Fatalf("EnsureDefaultKey failed: %v", err)
	}
	if defaultKey.ID == bound.ID || defaultKey.ProviderBinding != nil {
		t.Fatalf("expected a general default key, got %+v", defaultKey)
	}
	if err := service.DeleteKey(defaultKey.ID); err == nil {
		t.Fatal("expected deleting the last general key to fail even with a dedicated key present")
	}
	if err := service.DeleteKey(bound.ID); err != nil {
		t.Fatalf("expected dedicated key deletion to succeed: %v", err)
	}
}

func TestProviderKeyAllowlistCannotBeEdited(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service := NewCodexRelayKeyService()

	bound, _, err := service.EnsureProviderKey(ProviderKindCodex, 1, "Codex · A")
	if err != nil {
		t.Fatalf("EnsureProviderKey failed: %v", err)
	}
	if err := service.UpdateAllowedProviderIDs(bound.ID, []int64{1, 2}); err == nil {
		t.Fatal("expected Codex allowlist update on a dedicated key to fail")
	}
	if err := service.UpdateAllowedClaudeProviderIDs(bound.ID, []int64{1}); err == nil {
		t.Fatal("expected Claude allowlist update on a dedicated key to fail")
	}
}

func TestPruneProviderKeysRemovesOnlyMissingProvidersOfKind(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service := NewCodexRelayKeyService()

	general, err := service.CreateKey("general")
	if err != nil {
		t.Fatalf("CreateKey failed: %v", err)
	}
	keep, _, _ := service.EnsureProviderKey(ProviderKindCodex, 1, "Codex · keep")
	gone, _, _ := service.EnsureProviderKey(ProviderKindCodex, 2, "Codex · gone")
	claude, _, _ := service.EnsureProviderKey(ProviderKindClaude, 2, "Claude · other kind")

	removed, err := service.PruneProviderKeys(ProviderKindCodex, []int64{1})
	if err != nil {
		t.Fatalf("PruneProviderKeys failed: %v", err)
	}
	if len(removed) != 1 || removed[0] != gone.ID {
		t.Fatalf("expected only %q to be removed, got %v", gone.ID, removed)
	}
	for _, id := range []string{general.ID, keep.ID, claude.ID} {
		if _, err := service.GetKeyByID(id); err != nil {
			t.Fatalf("expected key %q to remain: %v", id, err)
		}
	}
}

func TestProviderBindingPersistsThroughJSON(t *testing.T) {
	var key CodexRelayKey
	if err := json.Unmarshal([]byte(`{"id":"k","key":"csk_x","enabled":true,"providerBinding":{"kind":"claude","providerId":7}}`), &key); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if key.ProviderBinding == nil || key.ProviderBinding.Kind != ProviderKindClaude || key.ProviderBinding.ProviderID != 7 {
		t.Fatalf("unexpected binding: %+v", key.ProviderBinding)
	}
}

func TestRelayKeyProviderAccessForBoundKey(t *testing.T) {
	providers := []Provider{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}}
	bound := &CodexRelayKey{
		AllowedProviderIDs: []int64{1, 2},
		ProviderBinding:    &RelayKeyProviderBinding{Kind: ProviderKindCodex, ProviderID: 2},
	}

	allowlist, restricted := relayKeyProviderAccess(bound, ProviderKindCodex)
	filtered, skipped := filterProvidersByAllowlist(allowlist, restricted, providers)
	if !restricted || len(filtered) != 1 || filtered[0].ID != 2 || skipped != 1 {
		t.Fatalf("bound kind: restricted %v filtered %+v skipped %d", restricted, filtered, skipped)
	}

	allowlist, restricted = relayKeyProviderAccess(bound, ProviderKindClaude)
	filtered, _ = filterProvidersByAllowlist(allowlist, restricted, providers)
	if !restricted || len(filtered) != 0 {
		t.Fatalf("other kind must be denied, got restricted %v filtered %+v", restricted, filtered)
	}

	general := &CodexRelayKey{AllowedProviderIDs: []int64{1}}
	allowlist, restricted = relayKeyProviderAccess(general, ProviderKindClaude)
	filtered, _ = filterProvidersByAllowlist(allowlist, restricted, providers)
	if restricted || len(filtered) != 2 {
		t.Fatalf("general key with empty Claude allowlist must stay unrestricted, got %v %+v", restricted, filtered)
	}
}
