package daemon

// Usage reports are snapshots, so a resumed turn adds its frozen claim-time
// baseline, never a fresh read that would double count a repeated report.
func mergeWaitUsage(baseline, current []TaskUsageEntry) []TaskUsageEntry {
	previous := make(map[string]TaskUsageEntry, len(baseline))
	for _, entry := range baseline {
		previous[entry.Provider+"\x00"+entry.Model] = entry
	}
	out := make([]TaskUsageEntry, 0, len(baseline)+len(current))
	for _, entry := range current {
		key := entry.Provider + "\x00" + entry.Model
		if before, ok := previous[key]; ok {
			entry.InputTokens += before.InputTokens
			entry.OutputTokens += before.OutputTokens
			entry.CacheReadTokens += before.CacheReadTokens
			entry.CacheWriteTokens += before.CacheWriteTokens
			if before.CostUSDTicks > 0 && entry.CostUSDTicks > 0 {
				entry.CostUSDTicks += before.CostUSDTicks
			} else {
				entry.CostUSDTicks = 0
			}
			delete(previous, key)
		}
		out = append(out, entry)
	}
	for _, entry := range baseline {
		if _, ok := previous[entry.Provider+"\x00"+entry.Model]; ok {
			out = append(out, entry)
		}
	}
	return out
}
