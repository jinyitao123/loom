package loom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

const budgetRemainingStateKey = "__budget_remaining"

func TestBudgetSurvivesResume(t *testing.T) {
	store := NewMemStore()
	executions := 0
	g := NewGraph("budget-survives-resume", "work",
		WithStepBudget(5),
		WithMaxIterations(100),
	)
	g.AddStep("work", func(_ context.Context, _ State) (State, error) {
		executions++
		if executions == 2 {
			return State{"__yield": true, "__yield_phase": "after_step"}, nil
		}
		return State{}, nil
	}, Always("work"))

	first, err := g.Run(context.Background(), State{}, store)
	if err != nil {
		t.Fatalf("initial Run error: %v", err)
	}
	if first.StopReason != StopYielded || executions != 2 {
		t.Fatalf("initial Run = stop %q executions %d, want yielded after 2", first.StopReason, executions)
	}
	assertBudgetRemaining(t, first.State, 3)

	resumed, err := g.Resume(context.Background(), first.RunID, State{}, store)
	assertBudgetStop(t, resumed, err)
	if executions != 5 {
		t.Fatalf("total executions = %d, want 5", executions)
	}
	assertBudgetRemaining(t, resumed.State, 0)
}

func TestBudgetExhaustedAtResume(t *testing.T) {
	store := NewMemStore()
	nextExecutions := 0
	g := NewGraph("budget-exhausted-at-resume", "wait", WithStepBudget(1))
	g.AddStep("wait", func(_ context.Context, _ State) (State, error) {
		return State{"__yield": true, "__yield_phase": "after_step"}, nil
	}, Always("next"))
	g.AddStep("next", func(_ context.Context, _ State) (State, error) {
		nextExecutions++
		return State{}, nil
	}, End())

	first, err := g.Run(context.Background(), State{}, store)
	if err != nil {
		t.Fatalf("initial Run error: %v", err)
	}
	if first.StopReason != StopYielded {
		t.Fatalf("initial stop reason = %q, want %q", first.StopReason, StopYielded)
	}
	assertBudgetRemaining(t, first.State, 0)

	resumed, err := g.Resume(context.Background(), first.RunID, State{}, store)
	assertBudgetStop(t, resumed, err)
	if nextExecutions != 0 {
		t.Fatalf("next step executions = %d, want 0", nextExecutions)
	}
}

func TestNoBudgetNoKey(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	g := NewGraph("no-budget-no-key", "a", WithCheckpointHistory(-1))
	g.AddStep("a", func(_ context.Context, _ State) (State, error) {
		return State{"a": true}, nil
	}, Always("b"))
	g.AddStep("b", func(_ context.Context, _ State) (State, error) {
		return State{"b": true}, nil
	}, End())

	result, err := g.Run(ctx, State{}, store)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	assertNoBudgetRemaining(t, "terminal state", result.State)

	historyKeys, err := store.List(ctx, "checkpoint:"+g.Name, result.RunID+"/")
	if err != nil {
		t.Fatalf("List history: %v", err)
	}
	if len(historyKeys) != 2 {
		t.Fatalf("history keys = %v, want 2 checkpoints", historyKeys)
	}
	for _, key := range append([]string{result.RunID}, historyKeys...) {
		cp := readBudgetCheckpoint(t, store, g.Name, key)
		assertNoBudgetRemaining(t, "checkpoint "+key, cp.State)
	}
}

func TestCtxCounterWinsOverState(t *testing.T) {
	store := NewMemStore()
	var remaining atomic.Int64
	remaining.Store(1)
	ctx := context.WithValue(context.Background(), budgetKey{}, &remaining)
	executions := 0
	g := NewGraph("ctx-budget-wins", "work",
		WithStepBudget(100),
		WithMaxIterations(200),
	)
	g.AddStep("work", func(_ context.Context, _ State) (State, error) {
		executions++
		return State{}, nil
	}, Always("work"))

	result, err := g.Run(ctx, State{budgetRemainingStateKey: int(50)}, store)
	assertBudgetStop(t, result, err)
	if executions != 1 {
		t.Fatalf("executions = %d, want 1 from context budget", executions)
	}
	if got := remaining.Load(); got != -1 {
		t.Fatalf("context counter = %d, want -1 after rejected second step", got)
	}
	latest := readBudgetCheckpoint(t, store, g.Name, result.RunID)
	assertBudgetRemaining(t, latest.State, 0)
}

func TestResumeAtInheritsBudget(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	executions := 0
	g := NewGraph("resume-at-inherits-budget", "work",
		WithStepBudget(5),
		WithCheckpointHistory(-1),
		WithMaxIterations(200),
	)
	g.AddStep("work", func(_ context.Context, state State) (State, error) {
		executions++
		return State{"n": numericInt64(state["n"]) + 1}, nil
	}, func(_ context.Context, state State) (string, error) {
		if numericInt64(state["n"]) >= numericInt64(state["limit"]) {
			return "", nil
		}
		return "work", nil
	})

	source, err := g.Run(ctx, State{"limit": int64(4)}, store)
	if err != nil {
		t.Fatalf("source Run error: %v", err)
	}
	if source.StopReason != StopCompleted || executions != 4 {
		t.Fatalf("source Run = stop %q executions %d, want completed after 4", source.StopReason, executions)
	}
	branchPoint := readBudgetCheckpoint(t, store, g.Name, fmt.Sprintf("%s/%012d", source.RunID, 2))
	assertBudgetRemaining(t, branchPoint.State, 3)

	before := executions
	inherited, err := g.ResumeAt(ctx, source.RunID, 2, State{"limit": int64(100)}, store)
	assertBudgetStop(t, inherited, err)
	if got := executions - before; got != 3 {
		t.Fatalf("inherited fork executions = %d, want 3", got)
	}
	assertBudgetRemaining(t, inherited.State, 0)

	overrides := []struct {
		name  string
		value any
	}{
		{name: "int64", value: int64(1)},
		{name: "int", value: int(1)},
		{name: "float64", value: float64(1)},
	}
	for _, tt := range overrides {
		t.Run("override_"+tt.name, func(t *testing.T) {
			before := executions
			fork, err := g.ResumeAt(ctx, source.RunID, 2, State{
				"limit":                 int64(100),
				budgetRemainingStateKey: tt.value,
			}, store)
			assertBudgetStop(t, fork, err)
			if got := executions - before; got != 1 {
				t.Fatalf("override fork executions = %d, want 1", got)
			}
			assertBudgetRemaining(t, fork.State, 0)
		})
	}
}

func TestBudgetCheckpointValue(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	g := NewGraph("budget-checkpoint-value", "a",
		WithStepBudget(5),
		WithCheckpointHistory(-1),
	)
	g.AddStep("a", func(_ context.Context, _ State) (State, error) {
		return State{"a": true}, nil
	}, Always("b"))
	g.AddStep("b", func(_ context.Context, _ State) (State, error) {
		return State{"b": true}, nil
	}, Always("fail"))
	g.AddStep("fail", func(_ context.Context, _ State) (State, error) {
		return nil, errors.New("boom")
	}, End())

	result, err := g.Run(ctx, State{}, store)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("Run error = %v, want boom", err)
	}
	if result.StopReason != StopError {
		t.Fatalf("stop reason = %q, want %q", result.StopReason, StopError)
	}

	keys, err := store.List(ctx, "checkpoint:"+g.Name, result.RunID+"/")
	if err != nil {
		t.Fatalf("List history: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("history keys = %v, want 3 checkpoints", keys)
	}
	for i, key := range keys {
		cp := readBudgetCheckpoint(t, store, g.Name, key)
		want := int64(5 - (i + 1))
		assertBudgetRemaining(t, cp.State, want)
		if cp.Seq != int64(i+1) {
			t.Errorf("checkpoint %q seq = %d, want %d", key, cp.Seq, i+1)
		}
	}
	errorCheckpoint := readBudgetCheckpoint(t, store, g.Name, keys[2])
	if errorCheckpoint.State["__failed_step"] != "fail" {
		t.Fatalf("error checkpoint failed step = %#v, want fail", errorCheckpoint.State["__failed_step"])
	}
	latest := readBudgetCheckpoint(t, store, g.Name, result.RunID)
	assertBudgetRemaining(t, latest.State, 2)
}

func assertBudgetStop(t *testing.T, result *RunResult, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected step budget error, got nil")
	}
	if result == nil {
		t.Fatalf("budget result is nil: %v", err)
	}
	if result.StopReason != StopBudget {
		t.Fatalf("stop reason = %q, want %q (error: %v)", result.StopReason, StopBudget, err)
	}
}

func assertBudgetRemaining(t *testing.T, state State, want int64) {
	t.Helper()
	value, ok := state[budgetRemainingStateKey]
	if !ok {
		t.Fatalf("state has no %q: %#v", budgetRemainingStateKey, state)
	}
	var got int64
	switch value := value.(type) {
	case int64:
		got = value
	case int:
		got = int64(value)
	case float64:
		got = int64(value)
	default:
		t.Fatalf("%s has unsupported type %T", budgetRemainingStateKey, value)
	}
	if got != want {
		t.Fatalf("%s = %d (%T), want %d", budgetRemainingStateKey, got, value, want)
	}
}

func assertNoBudgetRemaining(t *testing.T, label string, state State) {
	t.Helper()
	if value, ok := state[budgetRemainingStateKey]; ok {
		t.Fatalf("%s contains %q = %#v", label, budgetRemainingStateKey, value)
	}
}

func readBudgetCheckpoint(t *testing.T, store Store, graphName, key string) checkpoint {
	t.Helper()
	data, err := store.Get(context.Background(), "checkpoint:"+graphName, key)
	if err != nil {
		t.Fatalf("Get checkpoint %q: %v", key, err)
	}
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		t.Fatalf("unmarshal checkpoint %q: %v", key, err)
	}
	return cp
}

func numericInt64(value any) int64 {
	switch value := value.(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	default:
		return 0
	}
}
