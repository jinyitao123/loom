package loom

import "context"

// CheckpointPolicy controls how checkpoint failures are handled.
type CheckpointPolicy int

const (
	// CheckpointBestEffort logs checkpoint failures but does not abort.
	CheckpointBestEffort CheckpointPolicy = iota
	// CheckpointRequired aborts the graph if checkpoint fails.
	CheckpointRequired
)

// StepHook runs before or after each step execution.
type StepHook func(ctx context.Context, stepName string, state State) error

// HookPoints holds before/after hooks for graph execution.
type HookPoints struct {
	Before []StepHook
	After  []StepHook
}

// GraphOption configures a Graph at construction time.
type GraphOption func(*Graph)

func WithMergeConfig(cfg *MergeConfig) GraphOption {
	return func(g *Graph) { g.mergeConfig = cfg }
}

func WithCheckpointPolicy(p CheckpointPolicy) GraphOption {
	return func(g *Graph) { g.checkpointPolicy = p }
}

func WithMaxIterations(n int) GraphOption {
	return func(g *Graph) { g.maxIter = n }
}

// WithStepBudget sets a global step limit shared across parent and all sub-graphs.
func WithStepBudget(n int64) GraphOption {
	return func(g *Graph) { g.stepBudget = &n }
}
