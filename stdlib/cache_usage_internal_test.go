package stdlib

import (
	"github.com/jinyitao123/loom/contract"
	"testing"
)

func TestCacheUsageAggregationDoesNotInventMissingCoverage(t *testing.T) {
	initial, err := decodeToolLoopUsage(nil)
	if err != nil {
		t.Fatal(err)
	}
	known := addUsage(initial, contract.Usage{InputTokens: 100, CacheReadTokens: 80, CacheReadReported: true, CacheWriteReported: true})
	if !known.CacheReadReported || !known.CacheWriteReported || known.InputTokens != 100 {
		t.Fatalf("known usage=%+v", known)
	}
	partial := addUsage(known, contract.Usage{InputTokens: 20})
	if partial.CacheReadReported || partial.CacheWriteReported || partial.CacheReadTokens != 80 || partial.InputTokens != 120 {
		t.Fatalf("partial usage=%+v", partial)
	}
}
