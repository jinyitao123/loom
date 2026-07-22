package stdlib_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/stdlib"
)

func TestSubGraphStepReturnsChangedKeysAndSliceSuffix(t *testing.T) {
	store := loom.NewMemStore()
	childCfg := loom.DefaultMergeConfig()
	child := loom.NewGraph("delta-child", "change", loom.WithMergeConfig(childCfg))
	child.AddStep("change", func(_ context.Context, _ loom.State) (loom.State, error) {
		return loom.State{
			"patch":    "child",
			"messages": []any{"child-message"},
		}, nil
	}, loom.End())

	parentCfg := loom.DefaultMergeConfig()
	step := stdlib.NewSubGraphStep(child, store)
	delta, err := step(context.Background(), loom.State{
		"patch":     "parent",
		"unchanged": "keep",
		"messages":  []any{"parent-message"},
	})
	assertNoError(t, err)
	if _, ok := delta["unchanged"]; ok {
		t.Fatalf("delta contains unchanged key: %#v", delta)
	}
	if got := delta["messages"]; !reflect.DeepEqual(got, []any{"child-message"}) {
		t.Fatalf("messages delta = %#v, want child suffix", got)
	}

	parent := loom.NewGraph("delta-parent", "child", loom.WithMergeConfig(parentCfg))
	parent.AddStep("child", step, loom.End())

	result, err := parent.Run(context.Background(), loom.State{
		"patch":     "parent",
		"unchanged": "keep",
		"messages":  []any{"parent-message"},
	}, store)
	assertNoError(t, err)

	if got := result.State["patch"]; got != "child" {
		t.Fatalf("patch = %v, want child", got)
	}
	if got := result.State["unchanged"]; got != "keep" {
		t.Fatalf("unchanged = %v, want keep", got)
	}
	wantMessages := []any{"parent-message", "child-message"}
	if got := result.State["messages"]; !reflect.DeepEqual(got, wantMessages) {
		t.Fatalf("messages = %#v, want %#v", got, wantMessages)
	}
}

func TestSubGraphStepUsesParentMergeConfigForSumIntDelta(t *testing.T) {
	store := loom.NewMemStore()
	childCfg := loom.NewMergeConfig()
	childCfg.Register("count", loom.SumInt)
	child := loom.NewGraph("sum-child", "increment", loom.WithMergeConfig(childCfg))
	child.AddStep("increment", func(_ context.Context, _ loom.State) (loom.State, error) {
		return loom.State{"count": 3}, nil
	}, loom.End())

	parentCfg := loom.NewMergeConfig()
	parentCfg.Register("count", loom.SumInt)
	parent := loom.NewGraph("sum-parent", "child", loom.WithMergeConfig(parentCfg))
	parent.AddStep("child", stdlib.NewSubGraphStep(
		child,
		store,
		stdlib.WithParentMergeConfig(parentCfg),
	), loom.End())

	result, err := parent.Run(context.Background(), loom.State{"count": 10}, store)
	assertNoError(t, err)
	if got := result.State["count"]; got != 13 {
		t.Fatalf("count = %v, want 13", got)
	}
}

func TestSubGraphStepRejectsNonPrefixSliceForAppendPolicy(t *testing.T) {
	store := loom.NewMemStore()
	child := loom.NewGraph("reorder-child", "replace")
	child.AddStep("replace", func(_ context.Context, _ loom.State) (loom.State, error) {
		return loom.State{"messages": []any{"second", "first"}}, nil
	}, loom.End())

	parentCfg := loom.DefaultMergeConfig()
	step := stdlib.NewSubGraphStep(child, store, stdlib.WithParentMergeConfig(parentCfg))
	_, err := step(context.Background(), loom.State{"messages": []any{"first", "second"}})
	if err == nil {
		t.Fatal("expected non-prefix slice error")
	}
	if !strings.Contains(err.Error(), "messages") || !strings.Contains(err.Error(), "prefix") {
		t.Fatalf("error = %q, want key and prefix context", err)
	}
}
