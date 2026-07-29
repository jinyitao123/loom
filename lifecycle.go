package loom

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// RunAllocationEvent describes one run identity before any graph step or
// router is evaluated.
type RunAllocationEvent struct {
	RunID       string
	GraphName   string
	ParentRunID string
	ParentSeq   int64
	State       State
}

// RunAllocated synchronously admits one run identity before execution starts.
type RunAllocated interface {
	RunAllocated(context.Context, RunAllocationEvent) error
}

// RunExecutionContext optionally derives the context used by routers and
// steps after durable allocation. The returned context must inherit the input
// context so checkpoint observation and cancellation remain invocation-local.
type RunExecutionContext interface {
	ExecutionContext(context.Context, RunAllocationEvent) (context.Context, error)
}

// TerminalizationEvent describes the final observable state of one graph
// invocation.
type TerminalizationEvent struct {
	RunID                     string
	GraphName                 string
	StopReason                StopReason
	Yielded                   bool
	LastStep                  string
	State                     State
	LatestCheckpointSeq       int64
	LatestCheckpointPersisted bool
}

// Terminalizer synchronously observes one non-nil RunResult before it is
// returned to the caller.
type Terminalizer interface {
	Terminalize(context.Context, TerminalizationEvent) error
}

// CheckpointObservationStage identifies the latest-checkpoint boundary being
// observed. History writes are intentionally outside this lifecycle seam.
type CheckpointObservationStage string

const (
	CheckpointLatestPutBefore CheckpointObservationStage = "latest_put_before"
	CheckpointLatestPutAfter  CheckpointObservationStage = "latest_put_after"
)

// CheckpointEvent describes one latest-checkpoint persistence boundary.
type CheckpointEvent struct {
	Stage     CheckpointObservationStage
	RunID     string
	GraphName string
	Seq       int64
}

// CheckpointObserver synchronously observes latest-checkpoint persistence.
type CheckpointObserver interface {
	ObserveCheckpoint(context.Context, CheckpointEvent) error
}

// LifecycleHooks configures per-execution lifecycle callbacks. Hooks are
// passed to an invocation instead of stored on Graph so one Graph remains safe
// for concurrent callers with different lifecycle policies.
type LifecycleHooks struct {
	RunAllocated       RunAllocated
	ExecutionContext   RunExecutionContext
	Terminalizer       Terminalizer
	CheckpointObserver CheckpointObserver
}

type lifecycleCheckpointTracker struct {
	mu               sync.Mutex
	graphName        string
	runID            string
	observer         CheckpointObserver
	latestSeq        int64
	lastPutSucceeded bool
}

type lifecycleCheckpointTrackerKey struct{}

func newLifecycleCheckpointTracker(
	ctx context.Context,
	graphName string,
	runID string,
	observer CheckpointObserver,
) (
	context.Context,
	*lifecycleCheckpointTracker,
) {
	tracker := &lifecycleCheckpointTracker{
		graphName: graphName,
		runID:     runID,
		observer:  observer,
	}
	return context.WithValue(ctx, lifecycleCheckpointTrackerKey{}, tracker), tracker
}

func lifecycleCheckpointTrackerFrom(
	ctx context.Context,
	graphName string,
	runID string,
) *lifecycleCheckpointTracker {
	tracker, _ := ctx.Value(lifecycleCheckpointTrackerKey{}).(*lifecycleCheckpointTracker)
	if tracker == nil || tracker.graphName != graphName || tracker.runID != runID {
		return nil
	}
	return tracker
}

func (tracker *lifecycleCheckpointTracker) putSucceeded(seq int64) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.latestSeq = seq
	tracker.lastPutSucceeded = true
}

func (tracker *lifecycleCheckpointTracker) putAttempted(seq int64) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.latestSeq = seq
	tracker.lastPutSucceeded = false
}

func (tracker *lifecycleCheckpointTracker) putFailed() {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.lastPutSucceeded = false
}

func (tracker *lifecycleCheckpointTracker) snapshot() (int64, bool) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.latestSeq, tracker.lastPutSucceeded
}

func cloneLifecycleState(state State) State {
	if state == nil {
		return nil
	}
	cloned := make(State, len(state))
	for key, value := range state {
		cloned[key] = value
	}
	return cloned
}

func runAllocation(
	ctx context.Context,
	hooks LifecycleHooks,
	event RunAllocationEvent,
) (context.Context, error) {
	if hooks.RunAllocated != nil {
		callbackEvent := event
		callbackEvent.State = cloneLifecycleState(event.State)
		if err := hooks.RunAllocated.RunAllocated(ctx, callbackEvent); err != nil {
			return nil, fmt.Errorf(
				"loom: allocate run %q for graph %q: %w",
				event.RunID,
				event.GraphName,
				err,
			)
		}
	}
	if hooks.ExecutionContext == nil {
		return ctx, nil
	}
	executionEvent := event
	executionEvent.State = cloneLifecycleState(event.State)
	executionCtx, err := hooks.ExecutionContext.ExecutionContext(
		ctx,
		executionEvent,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"loom: derive execution context for run %q graph %q: %w",
			event.RunID,
			event.GraphName,
			err,
		)
	}
	if executionCtx == nil {
		return nil, fmt.Errorf(
			"loom: derive execution context for run %q graph %q: nil context",
			event.RunID,
			event.GraphName,
		)
	}
	return executionCtx, nil
}

func lifecycleParent(state State) (string, int64) {
	parentRunID, _ := state["__parent_run"].(string)
	var parentSeq int64
	switch value := state["__parent_seq"].(type) {
	case int64:
		parentSeq = value
	case int:
		parentSeq = int64(value)
	case float64:
		parentSeq = int64(value)
	}
	return parentRunID, parentSeq
}

func observeLatestCheckpoint(
	ctx context.Context,
	graphName string,
	runID string,
	seq int64,
	stage CheckpointObservationStage,
) error {
	tracker := lifecycleCheckpointTrackerFrom(ctx, graphName, runID)
	if tracker == nil || tracker.observer == nil {
		return nil
	}
	if err := tracker.observer.ObserveCheckpoint(ctx, CheckpointEvent{
		Stage:     stage,
		RunID:     runID,
		GraphName: graphName,
		Seq:       seq,
	}); err != nil {
		return fmt.Errorf(
			"loom: observe checkpoint %s for run %q graph %q seq %d: %w",
			stage,
			runID,
			graphName,
			seq,
			err,
		)
	}
	return nil
}

func runWithTerminalizer(
	ctx context.Context,
	graphName string,
	runID string,
	hooks LifecycleHooks,
	run func(context.Context) (*RunResult, error),
) (result *RunResult, err error) {
	trackedCtx, tracker := newLifecycleCheckpointTracker(
		ctx,
		graphName,
		runID,
		hooks.CheckpointObserver,
	)
	defer func() {
		if result == nil || hooks.Terminalizer == nil {
			return
		}
		latestSeq, latestPersisted := tracker.snapshot()
		terminalizeErr := hooks.Terminalizer.Terminalize(ctx, TerminalizationEvent{
			RunID:                     result.RunID,
			GraphName:                 graphName,
			StopReason:                result.StopReason,
			Yielded:                   result.Yielded,
			LastStep:                  result.LastStep,
			State:                     cloneLifecycleState(result.State),
			LatestCheckpointSeq:       latestSeq,
			LatestCheckpointPersisted: latestPersisted,
		})
		if terminalizeErr != nil {
			err = errors.Join(
				err,
				fmt.Errorf(
					"loom: terminalize run %q for graph %q: %w",
					result.RunID,
					graphName,
					terminalizeErr,
				),
			)
		}
	}()
	return run(trackedCtx)
}
