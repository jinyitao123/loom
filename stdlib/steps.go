package stdlib

import (
	"context"
	"fmt"

	"github.com/anthropic/loom"
)

// StepHook is a check function used by guard steps.
type StepHook func(ctx context.Context, stepName string, state loom.State) error

// NewGuardStep creates a step that runs guardrail checks.
func NewGuardStep(checks ...StepHook) loom.Step {
	return func(ctx context.Context, state loom.State) (loom.State, error) {
		for _, check := range checks {
			if err := check(ctx, "guard", state); err != nil {
				return loom.State{"__blocked": true, "__block_reason": err.Error()}, err
			}
		}
		return loom.State{}, nil
	}
}

// NewHumanWaitStep creates a step that yields for human input.
func NewHumanWaitStep(promptKey, responseKey string) loom.Step {
	return func(_ context.Context, state loom.State) (loom.State, error) {
		if _, ok := state[responseKey]; ok {
			return loom.State{}, nil
		}
		return loom.State{
			"__yield":       true,
			"__yield_phase": "mid_step",
		}, nil
	}
}

// NewLLMCallStep creates a single LLM call step (no tool loop).
func NewLLMCallStep(llm interface {
	Chat(ctx context.Context, req interface{}) (interface{}, error)
}) loom.Step {
	// Placeholder — real implementation uses contract.LLM.
	return func(ctx context.Context, state loom.State) (loom.State, error) {
		return loom.State{}, nil
	}
}

// YieldPolicy controls how a parent step handles a child graph's yield.
type YieldPolicy int

const (
	YieldBubble YieldPolicy = iota
	YieldTrap
	YieldCustom
)

// SubGraphOpts configures sub-graph step behavior.
type SubGraphOpts struct {
	YieldPolicy  YieldPolicy
	YieldHandler func(ctx context.Context, childResult *loom.RunResult) (loom.State, error)
}

// NewSubGraphStep creates a step that runs a child graph.
func NewSubGraphStep(child *loom.Graph, store loom.Store, opts ...SubGraphOpts) loom.Step {
	o := SubGraphOpts{YieldPolicy: YieldBubble}
	if len(opts) > 0 {
		o = opts[0]
	}

	return func(ctx context.Context, state loom.State) (loom.State, error) {
		result, err := child.Run(ctx, state, store)
		if err != nil {
			if result != nil {
				return result.State, err
			}
			return nil, err
		}

		if result.Yielded {
			switch o.YieldPolicy {
			case YieldBubble:
				return loom.State{
					"__yield":        true,
					"__yield_phase":  "mid_step",
					"__child_run_id": result.RunID,
					"__child_graph":  child.Name,
				}, nil
			case YieldTrap:
				return nil, fmt.Errorf("loom: sub-graph %q yielded but YieldTrap is set", child.Name)
			case YieldCustom:
				if o.YieldHandler != nil {
					return o.YieldHandler(ctx, result)
				}
				return nil, fmt.Errorf("loom: sub-graph %q yielded with YieldCustom but no handler", child.Name)
			}
		}

		return result.State, nil
	}
}

// ContextCompressor compresses conversation history for handoffs.
type ContextCompressor interface {
	Compress(msgs []interface{}) []interface{}
}

// NewHandoffStep creates a step that delegates to another graph with context compression.
func NewHandoffStep(target *loom.Graph, store loom.Store, compressor ContextCompressor) loom.Step {
	return func(ctx context.Context, state loom.State) (loom.State, error) {
		msgs, _ := GetMessages(state)
		var rawMsgs []interface{}
		for _, m := range msgs {
			rawMsgs = append(rawMsgs, m)
		}
		compressed := compressor.Compress(rawMsgs)
		handoffState := loom.State{"messages": compressed}
		for _, k := range []string{"session_id", "user_id", "namespace"} {
			if v, ok := state[k]; ok {
				handoffState[k] = v
			}
		}
		result, err := target.Run(ctx, handoffState, store)
		if err != nil {
			if result != nil {
				return result.State, err
			}
			return nil, err
		}

		if result.Yielded {
			return loom.State{
				"__yield":        true,
				"__yield_phase":  "mid_step",
				"__child_run_id": result.RunID,
				"__child_graph":  target.Name,
				"handoff_to":     target.Name,
			}, nil
		}

		return loom.State{
			"output":     result.State["output"],
			"handoff_to": target.Name,
		}, nil
	}
}
