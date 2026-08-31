package services

import (
	"testing"
	"time"

	"github.com/daodao97/xgo/xdb"
	"github.com/tidwall/gjson"
)

func TestRelayQuotaWindowUsesServerLocalCalendar(t *testing.T) {
	loc := time.FixedZone("CST", 8*60*60)
	cases := []struct {
		period    string
		current   time.Time
		wantStart string
		wantNext  string
	}{
		{RelayQuotaPeriodDaily, time.Date(2026, 8, 31, 18, 20, 0, 0, loc), "2026-08-31T00:00:00+08:00", "2026-09-01T00:00:00+08:00"},
		{RelayQuotaPeriodWeekly, time.Date(2026, 9, 2, 18, 20, 0, 0, loc), "2026-08-31T00:00:00+08:00", "2026-09-07T00:00:00+08:00"},
		{RelayQuotaPeriodMonthly, time.Date(2026, 8, 31, 18, 20, 0, 0, loc), "2026-08-01T00:00:00+08:00", "2026-09-01T00:00:00+08:00"},
	}
	for _, tc := range cases {
		start, next := relayQuotaWindow(tc.current, tc.period)
		if start.Format(time.RFC3339) != tc.wantStart || next.Format(time.RFC3339) != tc.wantNext {
			t.Fatalf("period %s: got %s -> %s", tc.period, start.Format(time.RFC3339), next.Format(time.RFC3339))
		}
	}
	start, next := relayQuotaWindow(time.Date(2026, 8, 31, 18, 20, 0, 0, loc), RelayQuotaPeriodOnce)
	if !next.IsZero() || start.Hour() != 18 {
		t.Fatalf("once period should not have a reset: start=%v next=%v", start, next)
	}
}

func TestRelayQuotaCostBreakdownAndSubsets(t *testing.T) {
	price := &relayModelPriceRecord{
		InputNano:       1_000_000_000,
		CachedInputNano: 100_000_000,
		OutputNano:      2_000_000_000,
		ReasoningNano:   4_000_000_000,
	}
	amount, priced, err := calculateRelayCostNanoUSD(RelayQuotaUsage{
		InputTokens:           1_000_000,
		CachedInputTokens:     200_000,
		OutputTokens:          500_000,
		ReasoningOutputTokens: 100_000,
	}, price)
	if err != nil {
		t.Fatalf("calculate cost: %v", err)
	}
	// 0.8M * $1 + 0.2M * $0.1 + 0.4M * $2 + 0.1M * $4 = $2.02
	if amount != 2_020_000_000 || !priced {
		t.Fatalf("amount=%d priced=%v, want 2020000000/true", amount, priced)
	}
	freeAmount, freePriced, err := calculateRelayCostNanoUSD(RelayQuotaUsage{InputTokens: 10}, &relayModelPriceRecord{})
	if err != nil || freeAmount != 0 || !freePriced {
		t.Fatalf("zero configured price should be free but priced: amount=%d priced=%v err=%v", freeAmount, freePriced, err)
	}
	unknownAmount, unknownPriced, err := calculateRelayCostNanoUSD(RelayQuotaUsage{InputTokens: 10}, nil)
	if err != nil || unknownAmount != 0 || unknownPriced {
		t.Fatalf("unknown price should be unpriced/free: amount=%d priced=%v err=%v", unknownAmount, unknownPriced, err)
	}
}

func TestEnsureChatCompletionUsageOptionPreservesOptions(t *testing.T) {
	body := []byte(`{"model":"m","stream":true,"stream_options":{"foo":"bar"}}`)
	updated := ensureChatCompletionUsageOption(body)
	if got := string(updated); got == string(body) {
		t.Fatal("expected stream_options to be added")
	}
	if !gjson.GetBytes(updated, "stream_options.include_usage").Bool() || gjson.GetBytes(updated, "stream_options.foo").String() != "bar" {
		t.Fatalf("stream options not preserved: %s", updated)
	}
	nonStream := []byte(`{"model":"m","stream":false}`)
	if string(ensureChatCompletionUsageOption(nonStream)) != string(nonStream) {
		t.Fatal("non-stream request should remain unchanged")
	}
}

func TestCodexUsageParserKeepsFinalCumulativeSSEUsage(t *testing.T) {
	var usage ReqeustLog
	parseEventPayload("data: {\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n"+
		"data: {\"response\":{\"usage\":{\"input_tokens\":20,\"output_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":3},\"output_tokens_details\":{\"reasoning_tokens\":2}}}}\n", CodexParseTokenUsageFromResponse, &usage)
	if usage.InputTokens != 20 || usage.OutputTokens != 5 || usage.CacheReadTokens != 3 || usage.ReasoningTokens != 2 {
		t.Fatalf("unexpected cumulative usage: %+v", usage)
	}
}

func TestCodexUsageParserAcceptsTopLevelUsageAliases(t *testing.T) {
	var usage ReqeustLog
	CodexParseTokenUsageFromResponse(`{"usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":3,"reasoning_tokens":2}}`, &usage)
	if usage.InputTokens != 10 || usage.OutputTokens != 4 || usage.CacheReadTokens != 3 || usage.ReasoningTokens != 2 {
		t.Fatalf("unexpected aliased usage: %+v", usage)
	}
}

func TestRequestLogHookParsesNonStreamingCodexJSON(t *testing.T) {
	var usage ReqeustLog
	ReqeustLogHook(nil, ProviderKindCodex, &usage)([]byte(`{"usage":{"input_tokens":7,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}`))
	if usage.InputTokens != 7 || usage.OutputTokens != 3 || usage.CacheReadTokens != 2 {
		t.Fatalf("non-stream usage was not parsed: %+v", usage)
	}
}

func TestRelayQuotaRoundTripAndIdempotency(t *testing.T) {
	db, err := xdb.DB("default")
	if err != nil {
		t.Setenv("HOME", t.TempDir())
		if initErr := InitDatabase(); initErr != nil {
			t.Skipf("database is not initialized: %v", initErr)
		}
		db, err = xdb.DB("default")
		if err != nil {
			t.Fatalf("database after initialization: %v", err)
		}
	}
	key := &CodexRelayKey{ID: "quota-test-" + time.Now().Format("150405.000000000"), TokenLimit: 100, USDLimit: "1", QuotaPeriod: RelayQuotaPeriodOnce}
	quota := NewRelayQuotaService()
	if err := ensureRelayQuotaTables(); err != nil {
		t.Fatalf("ensure quota tables: %v", err)
	}
	defer func() {
		_, _ = db.Exec("DELETE FROM relay_key_quota_state WHERE relay_key_id = ?", key.ID)
		_, _ = db.Exec("DELETE FROM relay_key_quota_settlement WHERE relay_key_id = ?", key.ID)
		_, _ = db.Exec("DELETE FROM relay_model_price WHERE model = ?", "quota-test-model")
	}()
	if _, err := quota.UpsertModelPrice(RelayModelPrice{Model: "quota-test-model", Input: "1", CachedInput: "0.1", Output: "2", ReasoningOutput: "4"}); err != nil {
		t.Fatalf("upsert price: %v", err)
	}
	decision, err := quota.Check(key)
	if err != nil || !decision.Allowed {
		t.Fatalf("initial check: allowed=%v err=%v", decision.Allowed, err)
	}
	usage := RelayQuotaUsage{InputTokens: 80, CachedInputTokens: 10, OutputTokens: 20, ReasoningOutputTokens: 5}
	first, err := quota.Settle(key, "quota-attempt-1", "provider", "quota-test-model", usage)
	if err != nil || first.AlreadySet || first.Amount == "0" {
		t.Fatalf("first settlement: %+v err=%v", first, err)
	}
	second, err := quota.Settle(key, "quota-attempt-1", "provider", "quota-test-model", usage)
	if err != nil || !second.AlreadySet {
		t.Fatalf("duplicate settlement: %+v err=%v", second, err)
	}
	status, err := quota.Status(key)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.TokenUsed != 100 || !status.TokenExhausted || status.USDUsed == "0" {
		t.Fatalf("unexpected status: %+v", status)
	}
	decision, err = quota.Check(key)
	if err != nil || decision.Allowed || decision.Reason != "token" {
		t.Fatalf("exhausted check: %+v err=%v", decision, err)
	}
}

func TestRelayQuotaUnpricedModelsResolveAfterPriceConfiguration(t *testing.T) {
	if _, err := xdb.DB("default"); err != nil {
		t.Setenv("HOME", t.TempDir())
		if initErr := InitDatabase(); initErr != nil {
			t.Skipf("database is not initialized: %v", initErr)
		}
	}
	quota := NewRelayQuotaService()
	key := &CodexRelayKey{ID: "unpriced-test-" + time.Now().Format("150405.000000000"), QuotaPeriod: RelayQuotaPeriodOnce}
	defer func() {
		db, _ := xdb.DB("default")
		_, _ = db.Exec("DELETE FROM relay_key_quota_settlement WHERE relay_key_id = ?", key.ID)
		_, _ = db.Exec("DELETE FROM relay_unpriced_model_seen WHERE model = ?", "unpriced-test-model")
		_, _ = db.Exec("DELETE FROM relay_model_price WHERE model = ?", "unpriced-test-model")
	}()
	if _, err := quota.Settle(key, "unpriced-attempt", "p", "unpriced-test-model", RelayQuotaUsage{InputTokens: 1}); err != nil {
		t.Fatalf("settle unknown: %v", err)
	}
	models, err := quota.ListUnpricedModels()
	if err != nil || !relayQuotaTestContainsUnpricedModel(models, "unpriced-test-model") {
		t.Fatalf("expected unpriced model, models=%v err=%v", models, err)
	}
	if _, err := quota.UpsertModelPrice(RelayModelPrice{Model: "unpriced-test-model", Input: "0", CachedInput: "0", Output: "0", ReasoningOutput: "0"}); err != nil {
		t.Fatalf("configure free price: %v", err)
	}
	models, err = quota.ListUnpricedModels()
	if err != nil || relayQuotaTestContainsUnpricedModel(models, "unpriced-test-model") {
		t.Fatalf("configured free model should resolve warning, models=%v err=%v", models, err)
	}
}

func relayQuotaTestContainsUnpricedModel(models []RelayUnpricedModel, target string) bool {
	for _, model := range models {
		if model.Model == target {
			return true
		}
	}
	return false
}

func TestRelayQuotaPeriodRollsAtBoundary(t *testing.T) {
	if _, err := xdb.DB("default"); err != nil {
		t.Setenv("HOME", t.TempDir())
		if initErr := InitDatabase(); initErr != nil {
			t.Skipf("database is not initialized: %v", initErr)
		}
	}
	loc := time.FixedZone("CST", 8*60*60)
	current := time.Date(2026, 8, 31, 23, 59, 59, 0, loc)
	quota := NewRelayQuotaService()
	quota.SetClockForTest(func() time.Time { return current }, func() *time.Location { return loc })
	key := &CodexRelayKey{ID: "boundary-test-" + time.Now().Format("150405.000000000"), TokenLimit: 10, QuotaPeriod: RelayQuotaPeriodDaily}
	defer func() {
		db, _ := xdb.DB("default")
		_, _ = db.Exec("DELETE FROM relay_key_quota_state WHERE relay_key_id = ?", key.ID)
		_, _ = db.Exec("DELETE FROM relay_key_quota_settlement WHERE relay_key_id = ?", key.ID)
		_, _ = db.Exec("DELETE FROM relay_unpriced_model_seen WHERE model = ?", "unknown-boundary-model")
	}()
	if _, err := quota.Settle(key, "boundary-attempt", "p", "unknown-boundary-model", RelayQuotaUsage{InputTokens: 5}); err != nil {
		t.Fatalf("settle before boundary: %v", err)
	}
	current = time.Date(2026, 9, 1, 0, 0, 1, 0, loc)
	status, err := quota.Status(key)
	if err != nil {
		t.Fatalf("status after boundary: %v", err)
	}
	if status.TokenUsed != 0 || status.WindowStartedAt != "2026-09-01T00:00:00+08:00" {
		t.Fatalf("quota did not roll over: %+v", status)
	}
}

func TestRelayQuotaManualResetUsesCalendarWindow(t *testing.T) {
	if _, err := xdb.DB("default"); err != nil {
		t.Setenv("HOME", t.TempDir())
		if initErr := InitDatabase(); initErr != nil {
			t.Skipf("database is not initialized: %v", initErr)
		}
	}
	loc := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 8, 19, 15, 42, 0, 0, loc)
	quota := NewRelayQuotaService()
	quota.SetClockForTest(func() time.Time { return now }, func() *time.Location { return loc })
	key := &CodexRelayKey{ID: "manual-reset-" + now.Format("150405.000000000"), TokenLimit: 100, QuotaPeriod: RelayQuotaPeriodWeekly}
	defer func() {
		db, _ := xdb.DB("default")
		_, _ = db.Exec("DELETE FROM relay_key_quota_state WHERE relay_key_id = ?", key.ID)
		_, _ = db.Exec("DELETE FROM relay_key_quota_settlement WHERE relay_key_id = ?", key.ID)
		_, _ = db.Exec("DELETE FROM relay_unpriced_model_seen WHERE model = ?", "unknown-manual-reset-model")
	}()
	if _, err := quota.Settle(key, "manual-reset-attempt", "p", "unknown-manual-reset-model", RelayQuotaUsage{InputTokens: 3}); err != nil {
		t.Fatalf("settle before reset: %v", err)
	}
	if err := quota.Reset(key); err != nil {
		t.Fatalf("reset: %v", err)
	}
	status, err := quota.Status(key)
	if err != nil {
		t.Fatalf("status after reset: %v", err)
	}
	if status.TokenUsed != 0 || status.WindowStartedAt != "2026-08-17T00:00:00+08:00" {
		t.Fatalf("manual reset did not retain calendar window: %+v", status)
	}
}

func TestRelayQuotaStatusDoesNotStartTracking(t *testing.T) {
	if _, err := xdb.DB("default"); err != nil {
		t.Setenv("HOME", t.TempDir())
		if initErr := InitDatabase(); initErr != nil {
			t.Skipf("database is not initialized: %v", initErr)
		}
	}
	loc := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 8, 19, 15, 42, 0, 0, loc)
	quota := NewRelayQuotaService()
	quota.SetClockForTest(func() time.Time { return now }, func() *time.Location { return loc })
	key := &CodexRelayKey{ID: "status-read-" + now.Format("150405.000000000"), TokenLimit: 100, QuotaPeriod: RelayQuotaPeriodMonthly}
	defer func() {
		db, _ := xdb.DB("default")
		_, _ = db.Exec("DELETE FROM relay_key_quota_state WHERE relay_key_id = ?", key.ID)
	}()
	status, err := quota.Status(key)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.TrackingStarted || status.WindowStartedAt != "2026-08-01T00:00:00+08:00" {
		t.Fatalf("status should be read-only before first call: %+v", status)
	}
	db, err := xdb.DB("default")
	if err != nil {
		t.Fatalf("quota db: %v", err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM relay_key_quota_state WHERE relay_key_id = ?", key.ID).Scan(&count); err != nil {
		t.Fatalf("state count: %v", err)
	}
	if count != 0 {
		t.Fatalf("status unexpectedly created state row: %d", count)
	}
}
