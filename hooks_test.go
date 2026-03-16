package loom_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/jinyitao123/loom"
)

func TestHook_Before_Aborts(t *testing.T) {
	g := loom.NewGraph("test", "s")
	g.AddStep("s", echoStep("x", "1"), loom.End())
	g.SetHooks(loom.HookPoints{
		Before: []loom.StepHook{
			func(_ context.Context, _ string, _ loom.State) error {
				return fmt.Errorf("blocked by guardrail")
			},
		},
	})

	_, err := g.Run(context.Background(), loom.State{}, nil)
	assertError(t, err, "blocked by guardrail")
}

func TestHook_After_Aborts(t *testing.T) {
	g := loom.NewGraph("test", "s")
	g.AddStep("s", echoStep("x", "1"), loom.End())
	g.SetHooks(loom.HookPoints{
		After: []loom.StepHook{
			func(_ context.Context, _ string, _ loom.State) error {
				return fmt.Errorf("output policy violation")
			},
		},
	})

	_, err := g.Run(context.Background(), loom.State{}, nil)
	assertError(t, err, "output policy violation")
}

func TestHook_ExecutionOrder(t *testing.T) {
	var order []string

	g := loom.NewGraph("test", "s")
	g.AddStep("s", func(_ context.Context, _ loom.State) (loom.State, error) {
		order = append(order, "step")
		return loom.State{}, nil
	}, loom.End())
	g.SetHooks(loom.HookPoints{
		Before: []loom.StepHook{
			func(_ context.Context, _ string, _ loom.State) error { order = append(order, "before1"); return nil },
			func(_ context.Context, _ string, _ loom.State) error { order = append(order, "before2"); return nil },
		},
		After: []loom.StepHook{
			func(_ context.Context, _ string, _ loom.State) error { order = append(order, "after1"); return nil },
			func(_ context.Context, _ string, _ loom.State) error { order = append(order, "after2"); return nil },
		},
	})

	g.Run(context.Background(), loom.State{}, nil)

	want := []string{"before1", "before2", "step", "after1", "after2"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestHook_ReceivesStepName(t *testing.T) {
	var receivedNames []string

	g := loom.NewGraph("test", "alpha")
	g.AddStep("alpha", echoStep("x", "1"), loom.Always("beta"))
	g.AddStep("beta", echoStep("y", "2"), loom.End())
	g.SetHooks(loom.HookPoints{
		Before: []loom.StepHook{
			func(_ context.Context, name string, _ loom.State) error {
				receivedNames = append(receivedNames, name)
				return nil
			},
		},
	})

	g.Run(context.Background(), loom.State{}, nil)

	want := []string{"alpha", "beta"}
	if !reflect.DeepEqual(receivedNames, want) {
		t.Errorf("hook received %v, want %v", receivedNames, want)
	}
}

func TestHook_CanReadState(t *testing.T) {
	var capturedScore float64

	g := loom.NewGraph("test", "s")
	g.AddStep("s", func(_ context.Context, _ loom.State) (loom.State, error) {
		return loom.State{"score": 0.95}, nil
	}, loom.End())
	g.SetHooks(loom.HookPoints{
		After: []loom.StepHook{
			func(_ context.Context, _ string, state loom.State) error {
				capturedScore, _ = state["score"].(float64)
				return nil
			},
		},
	})

	g.Run(context.Background(), loom.State{}, nil)

	if capturedScore != 0.95 {
		t.Errorf("score = %v, want 0.95", capturedScore)
	}
}
