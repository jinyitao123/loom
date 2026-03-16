package stdlib_test

import (
	"context"
	"testing"

	"github.com/anthropic/loom"
	"github.com/anthropic/loom/contract"
	"github.com/anthropic/loom/stdlib"
)

func TestCostBudgetHook_UnderBudget(t *testing.T) {
	hook := stdlib.CostBudgetHook(10.0)
	state := loom.State{"usage": contract.Usage{CostUSD: 2.50}}
	err := hook(context.Background(), "chat", state)
	assertNoError(t, err)
}

func TestCostBudgetHook_OverBudget(t *testing.T) {
	hook := stdlib.CostBudgetHook(1.0)

	state1 := loom.State{"usage": contract.Usage{CostUSD: 0.80}}
	assertNoError(t, hook(context.Background(), "step1", state1))

	state2 := loom.State{"usage": contract.Usage{CostUSD: 0.50}}
	err := hook(context.Background(), "step2", state2)
	assertError(t, err, "cost")
}

func TestTokenBudgetHook_OverBudget(t *testing.T) {
	hook := stdlib.TokenBudgetHook(1000)

	state1 := loom.State{"usage": contract.Usage{InputTokens: 400, OutputTokens: 300}}
	assertNoError(t, hook(context.Background(), "step1", state1))

	state2 := loom.State{"usage": contract.Usage{InputTokens: 200, OutputTokens: 200}}
	err := hook(context.Background(), "step2", state2)
	assertError(t, err, "tokens")
}
