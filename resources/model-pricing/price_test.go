package modelpricing

import (
	"math"
	"testing"
)

func TestSupplementalAstraPriceAndMissingModel(t *testing.T) {
	service, err := NewService()
	if err != nil {
		t.Fatal(err)
	}
	cost := service.CalculateCost("gpt-6-astra", UsageSnapshot{InputTokens: 200000, CacheReadTokens: 800000, OutputTokens: 60000, ReasoningTokens: 40000})
	if !cost.HasPricing || math.Abs(cost.TotalCost-7.8) > 1e-9 {
		t.Fatalf("bad Astra price: %+v", cost)
	}
	for _, name := range []string{"unknown-model", "gpt-6-astra-made-up", "claude-unknown[1m]"} {
		cost = service.CalculateCost(name, UsageSnapshot{InputTokens: 1000000})
		if cost.HasPricing || cost.TotalCost != 0 {
			t.Fatalf("unknown %s accidentally matched: %+v", name, cost)
		}
	}
}
