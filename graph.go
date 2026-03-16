package loom

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Graph is an executable composition of Steps and Routers.
type Graph struct {
	Name             string
	steps            map[string]Step
	routers          map[string]Router
	entry            string
	hooks            HookPoints
	mergeConfig      *MergeConfig
	checkpointPolicy CheckpointPolicy
	maxIter          int
	stepBudget       *int64
}

// StopReason classifies why a graph execution ended.
type StopReason string

const (
	StopCompleted StopReason = "completed"  // normal termination (router returned "")
	StopYielded   StopReason = "yielded"    // HITL pause (__yield)
	StopMaxIter   StopReason = "max_iter"   // per-graph circuit breaker
	StopBudget    StopReason = "budget"     // global step budget exhausted
	StopError     StopReason = "error"      // Step returned non-nil error
	StopHookAbort StopReason = "hook_abort" // Before/After hook returned error
)

// RunResult contains the final state and metadata of a graph execution.
type RunResult struct {
	State      State
	LastStep   string
	Yielded    bool
	Steps      int
	RunID      string
	StopReason StopReason
}

type checkpoint struct {
	RunID      string    `json:"run_id"`
	Graph      string    `json:"graph"`
	LastStep   string    `json:"last_step"`
	State      State     `json:"state"`
	YieldPhase string    `json:"yield_phase"`
	SavedAt    time.Time `json:"saved_at"`
}

type budgetKey struct{}

// NewGraph creates a new graph with the given entry step.
func NewGraph(name string, entry string, opts ...GraphOption) *Graph {
	g := &Graph{
		Name:             name,
		steps:            make(map[string]Step),
		routers:          make(map[string]Router),
		entry:            entry,
		maxIter:          100,
		checkpointPolicy: CheckpointBestEffort,
	}
	for _, opt := range opts {
		opt(g)
	}
	if g.mergeConfig != nil {
		g.mergeConfig.frozen = true
	}
	return g
}

// AddStep registers a step with an optional router for the "after" transition.
func (g *Graph) AddStep(name string, step Step, after Router) {
	g.steps[name] = step
	if after != nil {
		g.routers[name] = after
	}
}

// SetHooks attaches before/after hooks.
func (g *Graph) SetHooks(h HookPoints) { g.hooks = h }

// resolveRunID safely extracts or generates a run ID.
func resolveRunID(input State) string {
	if v, ok := input["__run_id"].(string); ok && v != "" {
		return v
	}
	id := uuid.New().String()
	input["__run_id"] = id
	return id
}

// Run executes the graph from the entry step to completion or yield.
func (g *Graph) Run(ctx context.Context, input State, store Store) (*RunResult, error) {
	runID := resolveRunID(input)

	// Initialize global step budget if set on this graph (root only).
	if g.stepBudget != nil {
		remaining := atomic.Int64{}
		remaining.Store(*g.stepBudget)
		ctx = context.WithValue(ctx, budgetKey{}, &remaining)
	}

	state := input
	// Clean residual yield keys (e.g. from checkpoint state on Resume).
	delete(state, "__yield")
	delete(state, "__yield_phase")

	current := g.entry
	steps := 0

	for current != "" {
		// Per-graph circuit breaker.
		steps++
		if steps > g.maxIter {
			return &RunResult{State: state, LastStep: current, Steps: steps, RunID: runID, StopReason: StopMaxIter},
				fmt.Errorf("loom: graph %q exceeded max iterations (%d)", g.Name, g.maxIter)
		}

		// Global step budget check (shared with sub-graphs).
		if budget, ok := ctx.Value(budgetKey{}).(*atomic.Int64); ok {
			if budget.Add(-1) < 0 {
				return &RunResult{State: state, LastStep: current, Steps: steps, RunID: runID, StopReason: StopBudget},
					fmt.Errorf("loom: global step budget exhausted in graph %q", g.Name)
			}
		}

		step, ok := g.steps[current]
		if !ok {
			return &RunResult{State: state, LastStep: current, RunID: runID, StopReason: StopError},
				fmt.Errorf("loom: step %q not found in graph %q", current, g.Name)
		}

		// Before hooks.
		for _, hook := range g.hooks.Before {
			if err := hook(ctx, current, state); err != nil {
				return &RunResult{State: state, LastStep: current, RunID: runID, StopReason: StopHookAbort}, err
			}
		}

		// Execute step.
		update, err := step(ctx, state)
		if err != nil {
			state = state.Merge(State{
				"__error": err.Error(), "__failed_step": current,
			}, g.mergeConfig)
			_ = g.doCheckpoint(ctx, store, runID, current, state, "")
			return &RunResult{State: state, LastStep: current, RunID: runID, StopReason: StopError}, err
		}
		state = state.Merge(update, g.mergeConfig)

		// After hooks.
		for _, hook := range g.hooks.After {
			if err := hook(ctx, current, state); err != nil {
				return &RunResult{State: state, LastStep: current, RunID: runID, StopReason: StopHookAbort}, err
			}
		}

		// Checkpoint.
		yieldPhase, _ := state["__yield_phase"].(string)
		if err := g.doCheckpoint(ctx, store, runID, current, state, yieldPhase); err != nil {
			return &RunResult{State: state, LastStep: current, RunID: runID, StopReason: StopError}, err
		}

		// Check yield (HITL pause).
		if yielded, _ := state["__yield"].(bool); yielded {
			delete(state, "__yield")
			return &RunResult{
				State: state, LastStep: current, Yielded: true,
				Steps: steps, RunID: runID, StopReason: StopYielded,
			}, nil
		}

		// Route to next step.
		router := g.routers[current]
		if router == nil {
			break
		}
		prevStep := current
		current, err = router(ctx, state)
		if err != nil {
			return &RunResult{State: state, LastStep: prevStep, RunID: runID, StopReason: StopError}, err
		}
		if current == "" {
			current = prevStep
			break
		}
	}

	return &RunResult{State: state, LastStep: current, Steps: steps, RunID: runID, StopReason: StopCompleted}, nil
}

// Resume restarts a yielded graph from its checkpoint.
func (g *Graph) Resume(ctx context.Context, runID string, input State, store Store) (*RunResult, error) {
	data, err := store.Get(ctx, "checkpoint:"+g.Name, runID)
	if err != nil {
		return nil, fmt.Errorf("loom: checkpoint not found for run %s: %w", runID, err)
	}
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("loom: corrupt checkpoint for run %s: %w", runID, err)
	}

	state := cp.State.Merge(input, g.mergeConfig)
	state["__run_id"] = runID

	switch cp.YieldPhase {
	case "after_step":
		router := g.routers[cp.LastStep]
		if router == nil {
			return &RunResult{State: state, RunID: runID}, nil
		}
		nextStep, err := router(ctx, state)
		if err != nil {
			return &RunResult{State: state, LastStep: cp.LastStep, RunID: runID}, err
		}
		if nextStep == "" {
			return &RunResult{State: state, LastStep: cp.LastStep, RunID: runID}, nil
		}
		return g.withEntry(nextStep).Run(ctx, state, store)

	default: // "mid_step" or empty (v1.0 compat)
		return g.withEntry(cp.LastStep).Run(ctx, state, store)
	}
}

// doCheckpoint handles persistence with configurable policy.
func (g *Graph) doCheckpoint(ctx context.Context, store Store, runID, step string, state State, yieldPhase string) error {
	if store == nil {
		return nil
	}
	cp := checkpoint{
		RunID:      runID,
		Graph:      g.Name,
		LastStep:   step,
		State:      state,
		YieldPhase: yieldPhase,
		SavedAt:    time.Now(),
	}
	data, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("loom: checkpoint marshal failed at step %q: %w (check State for non-JSON values)", step, err)
	}
	err = store.Put(ctx, "checkpoint:"+g.Name, runID, data)
	if err != nil {
		switch g.checkpointPolicy {
		case CheckpointRequired:
			return fmt.Errorf("loom: checkpoint failed at step %q: %w", step, err)
		default:
			slog.Warn("loom: checkpoint failed (best-effort)",
				"graph", g.Name, "step", step, "error", err)
			return nil
		}
	}
	return nil
}

// withEntry creates a shallow copy of the graph with a different entry point.
// The copy shares steps, routers, hooks, and mergeConfig but does NOT
// re-initialize the step budget (budget is inherited via context).
func (g *Graph) withEntry(newEntry string) *Graph {
	return &Graph{
		Name:             g.Name,
		steps:            g.steps,
		routers:          g.routers,
		entry:            newEntry,
		hooks:            g.hooks,
		mergeConfig:      g.mergeConfig,
		checkpointPolicy: g.checkpointPolicy,
		maxIter:          g.maxIter,
		stepBudget:       nil,
	}
}
