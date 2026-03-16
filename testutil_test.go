package loom_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/loom"
)

// --- Simple Steps for Testing ---

func echoStep(key, value string) loom.Step {
	return func(_ context.Context, _ loom.State) (loom.State, error) {
		return loom.State{key: value}, nil
	}
}

func counterStep(key string) loom.Step {
	return func(_ context.Context, state loom.State) (loom.State, error) {
		count, _ := state[key].(int)
		return loom.State{key: count + 1}, nil
	}
}

func failStep(msg string) loom.Step {
	return func(_ context.Context, _ loom.State) (loom.State, error) {
		return nil, fmt.Errorf("%s", msg)
	}
}

func yieldStep(phase string) loom.Step {
	return func(_ context.Context, _ loom.State) (loom.State, error) {
		return loom.State{
			"__yield":       true,
			"__yield_phase": phase,
		}, nil
	}
}

func collectStep(name string) loom.Step {
	return func(_ context.Context, _ loom.State) (loom.State, error) {
		return loom.State{"trace": []any{name}}, nil
	}
}

func delayStep(d time.Duration, key, value string) loom.Step {
	return func(ctx context.Context, _ loom.State) (loom.State, error) {
		select {
		case <-time.After(d):
			return loom.State{key: value}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// --- Assertions ---

func assertState(t *testing.T, result *loom.RunResult, key string, want any) {
	t.Helper()
	got := result.State[key]
	if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
		t.Errorf("state[%q] = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}

func assertYielded(t *testing.T, result *loom.RunResult, want bool) {
	t.Helper()
	if result.Yielded != want {
		t.Errorf("Yielded = %v, want %v", result.Yielded, want)
	}
}

func assertSteps(t *testing.T, result *loom.RunResult, want int) {
	t.Helper()
	if result.Steps != want {
		t.Errorf("Steps = %d, want %d", result.Steps, want)
	}
}

func assertLastStep(t *testing.T, result *loom.RunResult, want string) {
	t.Helper()
	if result.LastStep != want {
		t.Errorf("LastStep = %q, want %q", result.LastStep, want)
	}
}

func assertError(t *testing.T, err error, wantSubstring string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", wantSubstring)
	}
	if !strings.Contains(err.Error(), wantSubstring) {
		t.Errorf("error = %q, want substring %q", err.Error(), wantSubstring)
	}
}

func assertNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func assertTrace(t *testing.T, result *loom.RunResult, want ...string) {
	t.Helper()
	trace, _ := result.State["trace"].([]any)
	if len(trace) != len(want) {
		t.Fatalf("trace length = %d, want %d\n  got:  %v\n  want: %v", len(trace), len(want), trace, want)
	}
	for i, w := range want {
		if fmt.Sprintf("%v", trace[i]) != w {
			t.Errorf("trace[%d] = %v, want %v\n  full trace: %v", i, trace[i], w, trace)
		}
	}
}

// --- Failing Store ---

type failingStore struct {
	loom.Store
	failOn string
}

func (s *failingStore) Put(ctx context.Context, ns, key string, val []byte) error {
	if s.failOn == "Put" {
		return fmt.Errorf("store: simulated Put failure")
	}
	return s.Store.Put(ctx, ns, key, val)
}

func (s *failingStore) Get(ctx context.Context, ns, key string) ([]byte, error) {
	if s.failOn == "Get" {
		return nil, fmt.Errorf("store: simulated Get failure")
	}
	return s.Store.Get(ctx, ns, key)
}
