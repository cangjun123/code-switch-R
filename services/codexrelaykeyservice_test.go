package services

import (
	"encoding/json"
	"testing"
)

func TestCodexRelayKeyServiceCreateCopyAndDelete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	service := NewCodexRelayKeyService()

	firstKey, err := service.CreateKey("local-dev")
	if err != nil {
		t.Fatalf("CreateKey(first) failed: %v", err)
	}
	if firstKey.Key == "" {
		t.Fatal("expected first key secret to be returned")
	}

	list, err := service.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys() failed: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 key after first create, got %d", len(list))
	}
	if list[0].MaskedKey == "" || list[0].MaskedKey == firstKey.Key {
		t.Fatalf("expected masked key in list output, got %q", list[0].MaskedKey)
	}

	secret, err := service.GetKeySecret(firstKey.ID)
	if err != nil {
		t.Fatalf("GetKeySecret() failed: %v", err)
	}
	if secret != firstKey.Key {
		t.Fatalf("expected copied secret to match created key")
	}

	if err := service.DeleteKey(firstKey.ID); err == nil {
		t.Fatal("expected deleting the last enabled key to fail")
	}

	secondKey, err := service.CreateKey("ci")
	if err != nil {
		t.Fatalf("CreateKey(second) failed: %v", err)
	}

	if err := service.DeleteKey(firstKey.ID); err != nil {
		t.Fatalf("DeleteKey(first) after creating second key failed: %v", err)
	}

	list, err = service.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys() after delete failed: %v", err)
	}
	if len(list) != 1 || list[0].ID != secondKey.ID {
		t.Fatalf("expected remaining key %q, got %+v", secondKey.ID, list)
	}
}

func TestCodexRelayKeyQuotaJSONSnakeCaseCompatibility(t *testing.T) {
	var key CodexRelayKey
	if err := json.Unmarshal([]byte(`{"id":"k","key":"csk_test","enabled":true,"token_limit":123,"usd_limit":"4.5","quota_period":"monthly","allowed_provider_ids":[9,3,9]}`), &key); err != nil {
		t.Fatalf("unmarshal key: %v", err)
	}
	if key.TokenLimit != 123 || key.USDLimit != "4.5" || key.QuotaPeriod != "monthly" || !equalInt64Slices(key.AllowedProviderIDs, []int64{3, 9}) {
		t.Fatalf("unexpected key quota fields: %+v", key)
	}
}

func TestCodexRelayKeyQuotaJSONAcceptsNumericLimitsAndPeriodAlias(t *testing.T) {
	var key CodexRelayKey
	if err := json.Unmarshal([]byte(`{"id":"k","key":"csk_test","enabled":true,"tokenLimit":"123","usdLimit":4.5,"period":"daily"}`), &key); err != nil {
		t.Fatalf("unmarshal numeric key: %v", err)
	}
	if key.TokenLimit != 123 || key.USDLimit != "4.5" || key.QuotaPeriod != "daily" {
		t.Fatalf("unexpected numeric key quota fields: %+v", key)
	}
}

func TestCodexRelayKeyProviderAccessPersistsAndEmptyMeansUnrestricted(t *testing.T) {
	service := &CodexRelayKeyService{path: t.TempDir() + "/keys.json"}
	created, err := service.CreateKey("provider-access")
	if err != nil {
		t.Fatalf("CreateKey() failed: %v", err)
	}
	if err := service.UpdateAllowedProviderIDs(created.ID, []int64{7, 2, 7}); err != nil {
		t.Fatalf("UpdateAllowedProviderIDs() failed: %v", err)
	}
	key, err := service.GetKeyByID(created.ID)
	if err != nil {
		t.Fatalf("GetKeyByID() failed: %v", err)
	}
	if !equalInt64Slices(key.AllowedProviderIDs, []int64{2, 7}) {
		t.Fatalf("allowed provider IDs = %v, want [2 7]", key.AllowedProviderIDs)
	}
	list, err := service.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys() failed: %v", err)
	}
	if len(list) != 1 || !equalInt64Slices(list[0].AllowedProviderIDs, []int64{2, 7}) {
		t.Fatalf("unexpected key list: %+v", list)
	}
	if err := service.UpdateAllowedProviderIDs(created.ID, nil); err != nil {
		t.Fatalf("clear provider allowlist: %v", err)
	}
	key, err = service.GetKeyByID(created.ID)
	if err != nil {
		t.Fatalf("reload unrestricted key: %v", err)
	}
	if len(key.AllowedProviderIDs) != 0 {
		t.Fatalf("empty allowlist should mean unrestricted, got %v", key.AllowedProviderIDs)
	}
	if err := service.UpdateAllowedProviderIDs(created.ID, []int64{-1}); err == nil {
		t.Fatal("negative provider ID should be rejected")
	}
}

func TestCodexRelayKeyQuotaAllowsOnlyOneActiveLimit(t *testing.T) {
	service := &CodexRelayKeyService{path: t.TempDir() + "/keys.json"}
	created, err := service.CreateKey("single-quota")
	if err != nil {
		t.Fatalf("CreateKey() failed: %v", err)
	}
	if err := service.UpdateQuotaConfig(created.ID, 100, "2.5", RelayQuotaPeriodMonthly); err == nil {
		t.Fatal("expected simultaneous Token and USD limits to be rejected")
	}
	if err := service.UpdateQuotaConfig(created.ID, 100, "0", RelayQuotaPeriodWeekly); err != nil {
		t.Fatalf("save Token quota: %v", err)
	}
	if err := service.UpdateQuotaConfig(created.ID, 0, "2.5", RelayQuotaPeriodDaily); err != nil {
		t.Fatalf("save USD quota: %v", err)
	}
	key, err := service.GetKeyByID(created.ID)
	if err != nil {
		t.Fatalf("GetKeyByID() failed: %v", err)
	}
	if key.TokenLimit != 0 || key.USDLimit != "2.5" || key.QuotaPeriod != RelayQuotaPeriodDaily {
		t.Fatalf("unexpected saved quota: %+v", key)
	}
}
