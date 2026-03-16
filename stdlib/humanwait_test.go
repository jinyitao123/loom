package stdlib_test

import (
	"context"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/stdlib"
)

func TestHumanWait_YieldsWhenNoResponse(t *testing.T) {
	step := stdlib.NewHumanWaitStep("prompt", "response")
	result, _ := step(context.Background(), loom.State{})
	if result["__yield"] != true {
		t.Error("should yield")
	}
	if result["__yield_phase"] != "mid_step" {
		t.Error("should be mid_step")
	}
}

func TestHumanWait_PassThroughWhenResponseExists(t *testing.T) {
	step := stdlib.NewHumanWaitStep("prompt", "response")
	result, err := step(context.Background(), loom.State{"response": "approved"})
	assertNoError(t, err)
	if result["__yield"] != nil {
		t.Error("should NOT yield when response exists")
	}
}
