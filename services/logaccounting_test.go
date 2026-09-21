package services

import (
	"math"
	"testing"
	"time"

	modelpricing "codeswitch/resources/model-pricing"
	"github.com/daodao97/xgo/xdb"
)

func TestRequestLogUsageDoesNotDoubleCountDetails(t *testing.T) {
	tests := []struct {
		platform, model                        string
		input, output, reasoning, cache, write int
		wantInput, wantOutput                  int64
	}{
		{"codex", "gpt-6-astra", 100, 50, 20, 80, 0, 100, 50},
		{"gemini", "gemini-model", 100, 50, 20, 80, 0, 100, 70},
		{"claude", "claude-sonnet-4", 100, 50, 0, 80, 30, 210, 50},
		{"custom-cli", "gpt-6-astra", 100, 50, 20, 80, 0, 100, 50},
	}
	for _, tt := range tests {
		t.Run(tt.platform, func(t *testing.T) {
			entry := ReqeustLog{Platform: tt.platform, Model: tt.model, InputTokens: tt.input, OutputTokens: tt.output, ReasoningTokens: tt.reasoning, CacheReadTokens: tt.cache, CacheCreateTokens: tt.write}
			u := requestLogUsage(&entry)
			if usageInput(u) != tt.wantInput || usageOutput(u) != tt.wantOutput {
				t.Fatalf("input=%d output=%d usage=%+v", usageInput(u), usageOutput(u), u)
			}
		})
	}
}

func TestLogAccountingAstraCustomFreeAndUnknown(t *testing.T) {
	setupRelayTestEnv(t)
	db, err := xdb.DB("default")
	if err != nil {
		t.Fatal(err)
	}
	// Use the same day and rows for cards, details, provider totals and heatmap.
	if _, err = db.Exec(`DELETE FROM request_log`); err != nil {
		t.Fatal(err)
	}
	const custom = "accounting-custom-model"
	const free = "accounting-free-model"
	for _, model := range []string{custom, free, "gpt-6-astra"} {
		_, _ = db.Exec(`DELETE FROM relay_model_price WHERE model=?`, model)
		_, _ = db.Exec(`DELETE FROM relay_deleted_model_price WHERE model=?`, model)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM request_log WHERE provider='accounting-provider'`)
		_, _ = db.Exec(`DELETE FROM relay_model_price WHERE model IN (?,?)`, custom, free)
	})
	_, err = db.Exec(`INSERT INTO relay_model_price(model,input_price_nano,cached_input_price_nano,output_price_nano,reasoning_output_price_nano) VALUES (?,2000000000,200000000,4000000000,6000000000),(?,0,0,0,0)`, custom, free)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"gpt-6-astra", custom, free, "accounting-unknown-model"} {
		_, err = db.Exec(`INSERT INTO request_log(platform,model,provider,http_code,input_tokens,output_tokens,reasoning_tokens,cache_read_tokens) VALUES ('codex',?,'accounting-provider',200,1000000,100000,40000,800000)`, model)
		if err != nil {
			t.Fatal(err)
		}
	}
	ls := NewLogService()
	stats, err := ls.StatsSince("codex", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	// Astra: 200k*10/M + 800k*1/M + 100k*50/M = $7.80.
	// Custom: 200k*2/M + 800k*.2/M + 60k*4/M + 40k*6/M = $1.04.
	wantCost := 8.84
	if stats.TotalRequests != 4 || stats.InputTokens != 4000000 || stats.OutputTokens != 400000 || stats.CacheReadTokens != 3200000 || stats.UnpricedRequests != 1 {
		t.Fatalf("bad totals: %+v", stats)
	}
	if math.Abs(stats.CostTotal-wantCost) > 1e-9 {
		t.Fatalf("cost=%f want=%f", stats.CostTotal, wantCost)
	}
	var seriesCost float64
	var seriesRequests int64
	for _, bucket := range stats.Series {
		seriesCost += bucket.TotalCost
		seriesRequests += bucket.TotalRequests
	}
	if math.Abs(seriesCost-wantCost) > 1e-9 || seriesRequests != 4 {
		t.Fatalf("bad series cost=%f requests=%d", seriesCost, seriesRequests)
	}
	details, err := ls.ListRequestLogs("codex", "accounting-provider", 10, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range details {
		switch entry.Model {
		case "gpt-6-astra":
			if !entry.HasPricing || math.Abs(entry.TotalCost-7.8) > 1e-9 {
				t.Fatalf("astra=%+v", entry)
			}
		case free:
			if !entry.HasPricing || entry.TotalCost != 0 {
				t.Fatalf("free=%+v", entry)
			}
		case "accounting-unknown-model":
			if entry.HasPricing {
				t.Fatal("unknown model marked as priced")
			}
		}
	}
	providers, err := ls.ProviderDailyStats("codex", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || math.Abs(providers[0].CostTotal-wantCost) > 1e-9 || providers[0].UnpricedRequests != 1 {
		t.Fatalf("provider totals=%+v", providers)
	}
	cost, err := ls.CostSince(time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), "codex", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(cost-wantCost) > 1e-9 {
		t.Fatalf("CostSince=%f", cost)
	}
	heatmap, err := ls.HeatmapStats(1, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	var heatCost float64
	for _, bucket := range heatmap {
		heatCost += bucket.TotalCost
	}
	if math.Abs(heatCost-wantCost) > 1e-9 {
		t.Fatalf("heatmap=%f", heatCost)
	}
	// Deleted built-in prices must stay unpriced, rather than silently falling back.
	_, err = db.Exec(`INSERT INTO relay_deleted_model_price(model) VALUES ('gpt-6-astra')`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM relay_deleted_model_price WHERE model='gpt-6-astra'`) })
	stats, err = ls.StatsSince("codex", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if stats.UnpricedRequests != 2 || math.Abs(stats.CostTotal-1.04) > 1e-9 {
		t.Fatalf("deleted override ignored: %+v", stats)
	}
}

func TestLogAccountingPricesLongContextPerRequest(t *testing.T) {
	setupRelayTestEnv(t)
	db, _ := xdb.DB("default")
	const platform = "accounting-long-context"
	_, _ = db.Exec(`DELETE FROM request_log WHERE platform=?`, platform)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM request_log WHERE platform=?`, platform) })
	const model = "claude-sonnet-4-20250514[1m]"
	for i := 0; i < 3; i++ {
		if _, err := db.Exec(`INSERT INTO request_log(platform,model,http_code,input_tokens,output_tokens) VALUES (?,?,200,100000,1000)`, platform, model); err != nil {
			t.Fatal(err)
		}
	}
	ls := NewLogService()
	stats, err := ls.StatsSince(platform, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	pricing, _ := modelpricing.DefaultService()
	want := 3 * pricing.CalculateCost(model, modelpricing.UsageSnapshot{InputTokens: 100000, OutputTokens: 1000}).TotalCost
	if want <= 0 || math.Abs(stats.CostTotal-want) > 1e-9 {
		t.Fatalf("aggregated requests trigger incorrect tier: cost=%f want=%f", stats.CostTotal, want)
	}
}
