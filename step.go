package loom

import "context"

// Step is the atomic unit of execution.
// It receives the current state and returns the delta to merge.
// Returning a non-nil error aborts the graph.
type Step func(ctx context.Context, state State) (State, error)
