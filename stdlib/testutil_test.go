package stdlib_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
)

// echoStep returns a step that sets a key to a fixed value.
func echoStep(key string, value any) loom.Step {
	return func(_ context.Context, _ loom.State) (loom.State, error) {
		return loom.State{key: value}, nil
	}
}

// yieldStep returns a step that yields with the given phase.
func yieldStep(phase string) loom.Step {
	return func(_ context.Context, _ loom.State) (loom.State, error) {
		return loom.State{
			"__yield":       true,
			"__yield_phase": phase,
		}, nil
	}
}

func assertNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func assertError(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("error %q does not contain %q", err.Error(), substr)
	}
}
