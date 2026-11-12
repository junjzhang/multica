package daemon

import "testing"

func TestWaitUsagePreservesEarlierModelsWithoutDoubleCounting(t *testing.T) {
	baseline := []TaskUsageEntry{{Provider: "claude", Model: "earlier", InputTokens: 10, CostUSDTicks: 20}, {Provider: "claude", Model: "active", InputTokens: 30, CacheReadTokens: 5, CostUSDTicks: 40}}
	current := []TaskUsageEntry{{Provider: "claude", Model: "active", InputTokens: 7, CacheReadTokens: 2, CostUSDTicks: 10}}
	for i := 0; i < 2; i++ {
		merged := mergeWaitUsage(baseline, current)
		if len(merged) != 2 || merged[0].InputTokens != 37 || merged[0].CacheReadTokens != 7 || merged[0].CostUSDTicks != 50 || merged[1] != baseline[0] {
			t.Fatalf("usage lost or counted twice: %+v", merged)
		}
	}
	current[0].CostUSDTicks = 0
	merged := mergeWaitUsage(baseline, current)
	if merged[0].CostUSDTicks != 0 || baseline[1].InputTokens != 30 {
		t.Fatalf("invented cost or mutated snapshot: %+v %+v", merged, baseline)
	}
}
