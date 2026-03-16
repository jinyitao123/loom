package loom_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/anthropic/loom"
)

func assertStopReason(t *testing.T, result *loom.RunResult, want loom.StopReason) {
	t.Helper()
	if result.StopReason != want {
		t.Errorf("StopReason = %q, want %q", result.StopReason, want)
	}
}

func TestGraph_StopReason_Completed(t *testing.T) {
	g := loom.NewGraph("test", "s")
	g.AddStep("s", echoStep("x", "1"), loom.End())

	result, err := g.Run(context.Background(), loom.State{}, nil)
	assertNoError(t, err)
	assertStopReason(t, result, loom.StopCompleted)
}

func TestGraph_StopReason_Yielded(t *testing.T) {
	store := loom.NewMemStore()
	g := loom.NewGraph("test", "s")
	g.AddStep("s", yieldStep("mid_step"), loom.End())

	result, _ := g.Run(context.Background(), loom.State{}, store)
	assertStopReason(t, result, loom.StopYielded)
}

func TestGraph_StopReason_MaxIter(t *testing.T) {
	g := loom.NewGraph("test", "loop", loom.WithMaxIterations(3))
	g.AddStep("loop", counterStep("n"), loom.Always("loop"))

	result, err := g.Run(context.Background(), loom.State{}, nil)
	assertError(t, err, "exceeded max iterations")
	assertStopReason(t, result, loom.StopMaxIter)
}

func TestGraph_StopReason_Error(t *testing.T) {
	g := loom.NewGraph("test", "s")
	g.AddStep("s", failStep("something broke"), loom.End())

	result, _ := g.Run(context.Background(), loom.State{}, nil)
	assertStopReason(t, result, loom.StopError)
}

func TestGraph_StopReason_HookAbort(t *testing.T) {
	g := loom.NewGraph("test", "s")
	g.AddStep("s", echoStep("x", "1"), loom.End())
	g.SetHooks(loom.HookPoints{
		Before: []loom.StepHook{
			func(_ context.Context, _ string, _ loom.State) error {
				return fmt.Errorf("blocked")
			},
		},
	})

	result, _ := g.Run(context.Background(), loom.State{}, nil)
	assertStopReason(t, result, loom.StopHookAbort)
}

func TestGraph_StopReason_Budget(t *testing.T) {
	g := loom.NewGraph("test", "loop",
		loom.WithStepBudget(3),
		loom.WithMaxIterations(100),
	)
	g.AddStep("loop", counterStep("n"), loom.Always("loop"))

	result, err := g.Run(context.Background(), loom.State{}, nil)
	assertError(t, err, "step budget exhausted")
	assertStopReason(t, result, loom.StopBudget)
}
