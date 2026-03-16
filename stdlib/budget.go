package stdlib

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
)

// CostBudgetHook returns an AfterStep hook that tracks cumulative cost.
// When the budget is exceeded, it returns an error that stops the graph.
func CostBudgetHook(maxUSD float64) loom.StepHook {
	var totalCost float64
	var mu sync.Mutex
	return func(_ context.Context, _ string, state loom.State) error {
		mu.Lock()
		defer mu.Unlock()
		if usage, ok := state["usage"].(contract.Usage); ok {
			totalCost += usage.CostUSD
		}
		if totalCost > maxUSD {
			return fmt.Errorf("loom/budget: cost $%.4f exceeds limit $%.4f", totalCost, maxUSD)
		}
		return nil
	}
}

// TokenBudgetHook tracks cumulative input+output tokens.
func TokenBudgetHook(maxTokens int64) loom.StepHook {
	var totalTokens int64
	return func(_ context.Context, _ string, state loom.State) error {
		if usage, ok := state["usage"].(contract.Usage); ok {
			current := atomic.AddInt64(&totalTokens, int64(usage.InputTokens+usage.OutputTokens))
			if current > maxTokens {
				return fmt.Errorf("loom/budget: tokens %d exceeds limit %d", current, maxTokens)
			}
		}
		return nil
	}
}
