package loom_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/anthropic/loom"
)

func TestGraph_Yield_MidStep(t *testing.T) {
	store := loom.NewMemStore()
	g := loom.NewGraph("test", "a")
	g.AddStep("a", echoStep("x", "1"), loom.Always("b"))
	g.AddStep("b", yieldStep("mid_step"), loom.Always("c"))
	g.AddStep("c", echoStep("y", "2"), loom.End())

	result, err := g.Run(context.Background(), loom.State{}, store)
	assertNoError(t, err)
	assertYielded(t, result, true)
	assertLastStep(t, result, "b")
	assertState(t, result, "x", "1")
}

func TestGraph_Resume_MidStep_ReExecutes(t *testing.T) {
	store := loom.NewMemStore()

	execCount := 0
	g := loom.NewGraph("test", "wait")
	g.AddStep("wait", func(_ context.Context, state loom.State) (loom.State, error) {
		execCount++
		if _, ok := state["human_input"]; ok {
			return loom.State{"result": "got input"}, nil
		}
		return loom.State{"__yield": true, "__yield_phase": "mid_step"}, nil
	}, loom.End())

	result, _ := g.Run(context.Background(), loom.State{}, store)
	assertYielded(t, result, true)
	if execCount != 1 {
		t.Errorf("execCount = %d, want 1", execCount)
	}

	result, err := g.Resume(context.Background(), result.RunID, loom.State{"human_input": "yes"}, store)
	assertNoError(t, err)
	assertYielded(t, result, false)
	assertState(t, result, "result", "got input")
	if execCount != 2 {
		t.Errorf("execCount = %d, want 2 (re-executed)", execCount)
	}
}

func TestGraph_Resume_AfterStep_SkipsToNext(t *testing.T) {
	store := loom.NewMemStore()

	cfg := loom.DefaultMergeConfig()
	cfg.Register("trace", loom.AppendSlice)
	g := loom.NewGraph("test", "a", loom.WithMergeConfig(cfg))
	g.AddStep("a", collectStep("a"), loom.Always("b"))
	g.AddStep("b", func(_ context.Context, _ loom.State) (loom.State, error) {
		return loom.State{
			"trace":         []any{"b"},
			"__yield":       true,
			"__yield_phase": "after_step",
		}, nil
	}, loom.Always("c"))
	g.AddStep("c", collectStep("c"), loom.End())

	result, _ := g.Run(context.Background(), loom.State{}, store)
	assertYielded(t, result, true)

	result, err := g.Resume(context.Background(), result.RunID, loom.State{}, store)
	assertNoError(t, err)
	assertYielded(t, result, false)

	trace := result.State["trace"].([]any)
	bCount := 0
	for _, v := range trace {
		if v == "b" {
			bCount++
		}
	}
	if bCount != 1 {
		t.Errorf("step b executed %d times, want 1 (after_step should skip)", bCount)
	}
}

func TestGraph_Resume_NotFound(t *testing.T) {
	store := loom.NewMemStore()
	g := loom.NewGraph("test", "s")

	_, err := g.Resume(context.Background(), "nonexistent-run-id", loom.State{}, store)
	assertError(t, err, "checkpoint not found")
}

func TestGraph_Resume_CorruptCheckpoint(t *testing.T) {
	store := loom.NewMemStore()
	ctx := context.Background()
	store.Put(ctx, "checkpoint:test", "bad-run", []byte("not-valid-json"))

	g := loom.NewGraph("test", "s")
	_, err := g.Resume(ctx, "bad-run", loom.State{}, store)
	assertError(t, err, "corrupt checkpoint")
}

func TestGraph_DoubleYield(t *testing.T) {
	store := loom.NewMemStore()
	callCount := 0

	g := loom.NewGraph("test", "first_yield")
	g.AddStep("first_yield", func(_ context.Context, state loom.State) (loom.State, error) {
		callCount++
		if _, ok := state["input1"]; !ok {
			return loom.State{"__yield": true, "__yield_phase": "mid_step"}, nil
		}
		return loom.State{"got1": true}, nil
	}, loom.Always("second_yield"))

	g.AddStep("second_yield", func(_ context.Context, state loom.State) (loom.State, error) {
		callCount++
		if _, ok := state["input2"]; !ok {
			return loom.State{"__yield": true, "__yield_phase": "mid_step"}, nil
		}
		return loom.State{"got2": true}, nil
	}, loom.End())

	r1, _ := g.Run(context.Background(), loom.State{}, store)
	assertYielded(t, r1, true)
	assertLastStep(t, r1, "first_yield")

	r2, _ := g.Resume(context.Background(), r1.RunID, loom.State{"input1": "x"}, store)
	assertYielded(t, r2, true)
	assertLastStep(t, r2, "second_yield")

	r3, err := g.Resume(context.Background(), r2.RunID, loom.State{"input2": "y"}, store)
	assertNoError(t, err)
	assertYielded(t, r3, false)
	assertState(t, r3, "got1", true)
	assertState(t, r3, "got2", true)
}

func TestE2E_ReflectionLoop(t *testing.T) {
	store := loom.NewMemStore()
	cfg := loom.NewMergeConfig()
	cfg.Register("attempts", loom.SumInt)

	g := loom.NewGraph("reflect", "generate",
		loom.WithMergeConfig(cfg),
		loom.WithMaxIterations(20),
	)

	g.AddStep("generate", func(_ context.Context, state loom.State) (loom.State, error) {
		attempts, _ := state["attempts"].(int)
		score := float64(attempts) * 0.3
		return loom.State{
			"attempts": 1,
			"score":    score,
			"output":   fmt.Sprintf("attempt %d", attempts+1),
		}, nil
	}, loom.Always("evaluate"))

	g.AddStep("evaluate", func(_ context.Context, _ loom.State) (loom.State, error) {
		return loom.State{}, nil
	}, loom.Condition(
		func(s loom.State) bool { return s["score"].(float64) >= 0.7 },
		"",
		"generate",
	))

	result, err := g.Run(context.Background(), loom.State{}, store)
	assertNoError(t, err)

	attempts := result.State["attempts"].(int)
	score := result.State["score"].(float64)
	if attempts < 3 {
		t.Errorf("should need >= 3 attempts, got %d", attempts)
	}
	if score < 0.7 {
		t.Errorf("final score %f < 0.7", score)
	}
}

func TestE2E_CrashRecovery(t *testing.T) {
	store := loom.NewMemStore()
	cfg := loom.DefaultMergeConfig()
	cfg.Register("trace", loom.AppendSlice)

	callCount := 0
	g := loom.NewGraph("recover", "a",
		loom.WithMergeConfig(cfg),
		loom.WithCheckpointPolicy(loom.CheckpointRequired),
	)
	g.AddStep("a", collectStep("a"), loom.Always("b"))
	g.AddStep("b", func(_ context.Context, _ loom.State) (loom.State, error) {
		callCount++
		if callCount == 1 {
			return nil, fmt.Errorf("simulated crash")
		}
		return loom.State{"trace": []any{"b"}, "b_done": true}, nil
	}, loom.End())

	r1, err := g.Run(context.Background(), loom.State{}, store)
	if err == nil {
		t.Fatal("expected error")
	}

	r2, err := g.Resume(context.Background(), r1.RunID, loom.State{}, store)
	assertNoError(t, err)
	assertState(t, r2, "b_done", true)
}
