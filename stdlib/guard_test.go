package stdlib_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/anthropic/loom"
	"github.com/anthropic/loom/stdlib"
)

func TestGuardStep_AllPass(t *testing.T) {
	passCheck := func(_ context.Context, _ string, _ loom.State) error { return nil }

	step := stdlib.NewGuardStep(passCheck, passCheck)
	result, err := step(context.Background(), loom.State{"input": "hello"})
	assertNoError(t, err)
	if result["__blocked"] != nil {
		t.Error("should not be blocked")
	}
}

func TestGuardStep_FirstCheckFails(t *testing.T) {
	failCheck := func(_ context.Context, _ string, _ loom.State) error {
		return fmt.Errorf("prompt injection detected")
	}
	passCheck := func(_ context.Context, _ string, _ loom.State) error { return nil }

	step := stdlib.NewGuardStep(failCheck, passCheck)
	result, err := step(context.Background(), loom.State{})
	assertError(t, err, "prompt injection")
	if result["__blocked"] != true {
		t.Error("should be blocked")
	}
}

func TestGuardStep_Empty(t *testing.T) {
	step := stdlib.NewGuardStep() // no checks
	_, err := step(context.Background(), loom.State{})
	assertNoError(t, err)
}
