package stdlib_test

import (
	"context"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/stdlib"
)

func TestSubGraph_YieldBubble(t *testing.T) {
	store := loom.NewMemStore()
	child := loom.NewGraph("child", "wait")
	child.AddStep("wait", yieldStep("mid_step"), loom.End())

	step := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{YieldPolicy: stdlib.YieldBubble})
	result, err := step(context.Background(), loom.State{})
	assertNoError(t, err)
	if result["__yield"] != true {
		t.Error("yield should bubble up")
	}
	if result["__child_graph"] != "child" {
		t.Error("missing child graph name")
	}
}

func TestSubGraph_YieldTrap(t *testing.T) {
	store := loom.NewMemStore()
	child := loom.NewGraph("child", "wait")
	child.AddStep("wait", yieldStep("mid_step"), loom.End())

	step := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{YieldPolicy: stdlib.YieldTrap})
	_, err := step(context.Background(), loom.State{})
	assertError(t, err, "yielded but YieldTrap is set")
}

func TestSubGraph_NoYield_PassThrough(t *testing.T) {
	store := loom.NewMemStore()
	child := loom.NewGraph("child", "s")
	child.AddStep("s", echoStep("child_output", "hello"), loom.End())

	step := stdlib.NewSubGraphStep(child, store)
	result, err := step(context.Background(), loom.State{})
	assertNoError(t, err)
	if result["child_output"] != "hello" {
		t.Errorf("got %v", result["child_output"])
	}
}
