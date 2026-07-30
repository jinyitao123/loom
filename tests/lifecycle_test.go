package tests

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/stdlib"
)

type lifecycleRunAllocatedFunc func(context.Context, loom.RunAllocationEvent) error

func (fn lifecycleRunAllocatedFunc) RunAllocated(
	ctx context.Context,
	event loom.RunAllocationEvent,
) error {
	return fn(ctx, event)
}

type lifecycleExecutionContextFunc func(
	context.Context,
	loom.RunAllocationEvent,
) (context.Context, error)

func (fn lifecycleExecutionContextFunc) ExecutionContext(
	ctx context.Context,
	event loom.RunAllocationEvent,
) (context.Context, error) {
	return fn(ctx, event)
}

type lifecycleTerminalizerFunc func(context.Context, loom.TerminalizationEvent) error

func (fn lifecycleTerminalizerFunc) Terminalize(
	ctx context.Context,
	event loom.TerminalizationEvent,
) error {
	return fn(ctx, event)
}

type lifecycleCheckpointObserverFunc func(context.Context, loom.CheckpointEvent) error

func (fn lifecycleCheckpointObserverFunc) ObserveCheckpoint(
	ctx context.Context,
	event loom.CheckpointEvent,
) error {
	return fn(ctx, event)
}

type lifecyclePutFaultStore struct {
	loom.Store
	failAt   int
	putCalls int
	err      error
}

func (store *lifecyclePutFaultStore) Put(
	ctx context.Context,
	namespace string,
	key string,
	value []byte,
) error {
	store.putCalls++
	if store.putCalls == store.failAt {
		return store.err
	}
	return store.Store.Put(ctx, namespace, key, value)
}

func TestRunWithLifecycleAllocatesBeforeStep(t *testing.T) {
	var order []string
	var allocated loom.RunAllocationEvent
	var terminalized loom.TerminalizationEvent
	graph := loom.NewGraph("lifecycle-allocation", "work")
	graph.AddStep("work", func(_ context.Context, state loom.State) (loom.State, error) {
		order = append(order, "step")
		if _, leaked := state["hook_mutation"]; leaked {
			t.Fatal("allocation callback mutated execution state")
		}
		return loom.State{"output": "done"}, nil
	}, loom.End())

	result, err := graph.RunWithLifecycle(
		context.Background(),
		loom.State{"__run_id": "run-example-allocated", "input": "ready"},
		nil,
		loom.LifecycleHooks{
			RunAllocated: lifecycleRunAllocatedFunc(func(
				_ context.Context,
				event loom.RunAllocationEvent,
			) error {
				order = append(order, "allocate")
				allocated = event
				event.State["hook_mutation"] = true
				return nil
			}),
			Terminalizer: lifecycleTerminalizerFunc(func(
				_ context.Context,
				event loom.TerminalizationEvent,
			) error {
				order = append(order, "terminalize")
				terminalized = event
				return nil
			}),
		},
	)
	if err != nil {
		t.Fatalf("RunWithLifecycle: %v", err)
	}
	if result == nil {
		t.Fatal("RunWithLifecycle returned nil result")
	}
	if !reflect.DeepEqual(order, []string{"allocate", "step", "terminalize"}) {
		t.Fatalf("callback order = %#v", order)
	}
	if allocated.RunID != "run-example-allocated" ||
		allocated.GraphName != "lifecycle-allocation" ||
		allocated.ParentRunID != "" ||
		allocated.ParentSeq != 0 ||
		allocated.State["input"] != "ready" {
		t.Fatalf("allocation event = %#v", allocated)
	}
	if result.RunID != allocated.RunID ||
		terminalized.RunID != result.RunID ||
		terminalized.StopReason != loom.StopCompleted ||
		terminalized.State["output"] != "done" {
		t.Fatalf("result=%#v terminalization=%#v", result, terminalized)
	}
}

func TestLifecycleTerminalizesEveryNonNilRunResultExactlyOnce(t *testing.T) {
	runErr := errors.New("step failed")
	hookErr := errors.New("hook failed")
	routeErr := errors.New("route failed")
	storeErr := errors.New("checkpoint failed")

	tests := []struct {
		name  string
		build func() (*loom.Graph, loom.Store)
	}{
		{
			name: "completed",
			build: func() (*loom.Graph, loom.Store) {
				graph := loom.NewGraph("terminal-completed", "work")
				graph.AddStep("work", lifecycleNoopStep, loom.End())
				return graph, nil
			},
		},
		{
			name: "yielded",
			build: func() (*loom.Graph, loom.Store) {
				graph := loom.NewGraph("terminal-yielded", "wait")
				graph.AddStep("wait", func(context.Context, loom.State) (loom.State, error) {
					return loom.State{"__yield": true, "__yield_phase": "mid_step"}, nil
				}, loom.End())
				return graph, loom.NewMemStore()
			},
		},
		{
			name: "max iterations",
			build: func() (*loom.Graph, loom.Store) {
				return loom.NewGraph(
					"terminal-max-iterations",
					"work",
					loom.WithMaxIterations(0),
				), nil
			},
		},
		{
			name: "step budget",
			build: func() (*loom.Graph, loom.Store) {
				return loom.NewGraph(
					"terminal-budget",
					"work",
					loom.WithStepBudget(0),
				), nil
			},
		},
		{
			name: "missing step",
			build: func() (*loom.Graph, loom.Store) {
				return loom.NewGraph("terminal-missing", "missing"), nil
			},
		},
		{
			name: "before hook",
			build: func() (*loom.Graph, loom.Store) {
				graph := loom.NewGraph("terminal-before-hook", "work")
				graph.AddStep("work", lifecycleNoopStep, loom.End())
				graph.SetHooks(loom.HookPoints{Before: []loom.StepHook{
					func(context.Context, string, loom.State) error { return hookErr },
				}})
				return graph, nil
			},
		},
		{
			name: "step error",
			build: func() (*loom.Graph, loom.Store) {
				graph := loom.NewGraph("terminal-step-error", "work")
				graph.AddStep("work", func(context.Context, loom.State) (loom.State, error) {
					return nil, runErr
				}, loom.End())
				return graph, nil
			},
		},
		{
			name: "after hook",
			build: func() (*loom.Graph, loom.Store) {
				graph := loom.NewGraph("terminal-after-hook", "work")
				graph.AddStep("work", lifecycleNoopStep, loom.End())
				graph.SetHooks(loom.HookPoints{After: []loom.StepHook{
					func(context.Context, string, loom.State) error { return hookErr },
				}})
				return graph, nil
			},
		},
		{
			name: "required checkpoint",
			build: func() (*loom.Graph, loom.Store) {
				base := loom.NewMemStore()
				store := &lifecyclePutFaultStore{
					Store:  base,
					failAt: 1,
					err:    storeErr,
				}
				graph := loom.NewGraph(
					"terminal-checkpoint",
					"work",
					loom.WithCheckpointPolicy(loom.CheckpointRequired),
				)
				graph.AddStep("work", lifecycleNoopStep, loom.End())
				return graph, store
			},
		},
		{
			name: "router error",
			build: func() (*loom.Graph, loom.Store) {
				graph := loom.NewGraph("terminal-router", "work")
				graph.AddStep(
					"work",
					lifecycleNoopStep,
					func(context.Context, loom.State) (string, error) {
						return "", routeErr
					},
				)
				return graph, nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			graph, store := test.build()
			calls := 0
			var event loom.TerminalizationEvent
			result, _ := graph.RunWithLifecycle(
				context.Background(),
				loom.State{"__run_id": "run-" + test.name},
				store,
				loom.LifecycleHooks{
					Terminalizer: lifecycleTerminalizerFunc(func(
						_ context.Context,
						got loom.TerminalizationEvent,
					) error {
						calls++
						event = got
						return nil
					}),
				},
			)
			if result == nil {
				t.Fatal("RunWithLifecycle returned nil result")
			}
			if calls != 1 {
				t.Fatalf("terminalizer calls = %d, want 1", calls)
			}
			if event.RunID != result.RunID ||
				event.GraphName != graph.Name ||
				event.StopReason != result.StopReason ||
				event.Yielded != result.Yielded ||
				event.LastStep != result.LastStep {
				t.Fatalf("result=%#v terminalization=%#v", result, event)
			}
		})
	}
}

func TestLifecycleJoinsGraphAndTerminalizerErrors(t *testing.T) {
	graphErr := errors.New("graph error")
	terminalizerErr := errors.New("terminalizer error")
	graph := loom.NewGraph("lifecycle-error-join", "fail")
	graph.AddStep("fail", func(context.Context, loom.State) (loom.State, error) {
		return nil, graphErr
	}, loom.End())
	calls := 0

	result, err := graph.RunWithLifecycle(
		context.Background(),
		loom.State{"__run_id": "run-error-join"},
		nil,
		loom.LifecycleHooks{
			Terminalizer: lifecycleTerminalizerFunc(func(
				context.Context,
				loom.TerminalizationEvent,
			) error {
				calls++
				return terminalizerErr
			}),
		},
	)
	if result == nil {
		t.Fatal("RunWithLifecycle returned nil result")
	}
	if calls != 1 {
		t.Fatalf("terminalizer calls = %d, want 1", calls)
	}
	if !errors.Is(err, graphErr) || !errors.Is(err, terminalizerErr) {
		t.Fatalf("joined error = %v", err)
	}
}

func TestResumeWithLifecycleAfterStepReportsCompleted(t *testing.T) {
	store := loom.NewMemStore()
	graph := loom.NewGraph("lifecycle-resume-after-step", "wait")
	graph.AddStep("wait", func(context.Context, loom.State) (loom.State, error) {
		return loom.State{"__yield": true, "__yield_phase": "after_step"}, nil
	}, loom.End())
	first, err := graph.Run(
		context.Background(),
		loom.State{"__run_id": "run-resume-after-step"},
		store,
	)
	if err != nil || first == nil || !first.Yielded {
		t.Fatalf("seed result=%#v error=%v", first, err)
	}
	terminalizerCalls := 0
	var terminalized loom.TerminalizationEvent

	resumed, err := graph.ResumeWithLifecycle(
		context.Background(),
		first.RunID,
		loom.State{"approved": true},
		store,
		loom.LifecycleHooks{
			Terminalizer: lifecycleTerminalizerFunc(func(
				_ context.Context,
				event loom.TerminalizationEvent,
			) error {
				terminalizerCalls++
				terminalized = event
				return nil
			}),
		},
	)
	if err != nil {
		t.Fatalf("ResumeWithLifecycle: %v", err)
	}
	if resumed == nil ||
		terminalizerCalls != 1 ||
		resumed.StopReason != loom.StopCompleted ||
		terminalized.StopReason != loom.StopCompleted ||
		terminalized.RunID != first.RunID {
		t.Fatalf("result=%#v terminalization=%#v", resumed, terminalized)
	}
}

func TestResumeWithLifecycleAfterStepRouterErrorTerminalizesOnce(t *testing.T) {
	store := loom.NewMemStore()
	routerErr := errors.New("resume router unavailable")
	graph := loom.NewGraph("lifecycle-resume-after-step-error", "wait")
	graph.AddStep("wait", func(context.Context, loom.State) (loom.State, error) {
		return loom.State{"__yield": true, "__yield_phase": "after_step"}, nil
	}, func(context.Context, loom.State) (string, error) {
		return "", routerErr
	})
	first, err := graph.Run(
		context.Background(),
		loom.State{"__run_id": "run-resume-after-step-error"},
		store,
	)
	if err != nil || first == nil || !first.Yielded {
		t.Fatalf("seed result=%#v error=%v", first, err)
	}
	terminalizerCalls := 0
	var terminalized loom.TerminalizationEvent

	resumed, err := graph.ResumeWithLifecycle(
		context.Background(),
		first.RunID,
		loom.State{"approved": true},
		store,
		loom.LifecycleHooks{
			Terminalizer: lifecycleTerminalizerFunc(func(
				_ context.Context,
				event loom.TerminalizationEvent,
			) error {
				terminalizerCalls++
				terminalized = event
				return nil
			}),
		},
	)
	if resumed == nil ||
		terminalizerCalls != 1 ||
		resumed.StopReason != loom.StopError ||
		terminalized.StopReason != loom.StopError ||
		terminalized.RunID != first.RunID ||
		!errors.Is(err, routerErr) {
		t.Fatalf(
			"result=%#v terminalization=%#v calls=%d error=%v",
			resumed,
			terminalized,
			terminalizerCalls,
			err,
		)
	}
}

func TestLifecycleLatestCheckpointObservationAndStatus(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		store := loom.NewMemStore()
		graph := loom.NewGraph(
			"lifecycle-checkpoint-success",
			"work",
			loom.WithCheckpointPolicy(loom.CheckpointRequired),
		)
		graph.AddStep("work", lifecycleNoopStep, loom.End())
		var checkpoints []loom.CheckpointEvent
		var terminalized loom.TerminalizationEvent

		result, err := graph.RunWithLifecycle(
			context.Background(),
			loom.State{"__run_id": "run-checkpoint-success"},
			store,
			loom.LifecycleHooks{
				CheckpointObserver: lifecycleCheckpointObserverFunc(func(
					_ context.Context,
					event loom.CheckpointEvent,
				) error {
					checkpoints = append(checkpoints, event)
					return nil
				}),
				Terminalizer: lifecycleTerminalizerFunc(func(
					_ context.Context,
					event loom.TerminalizationEvent,
				) error {
					terminalized = event
					return nil
				}),
			},
		)
		if err != nil || result == nil {
			t.Fatalf("result=%#v error=%v", result, err)
		}
		want := []loom.CheckpointEvent{
			{
				Stage:     loom.CheckpointLatestPutBefore,
				RunID:     result.RunID,
				GraphName: graph.Name,
				Seq:       1,
			},
			{
				Stage:     loom.CheckpointLatestPutAfter,
				RunID:     result.RunID,
				GraphName: graph.Name,
				Seq:       1,
			},
		}
		if !reflect.DeepEqual(checkpoints, want) {
			t.Fatalf("checkpoint events = %#v, want %#v", checkpoints, want)
		}
		if terminalized.LatestCheckpointSeq != 1 ||
			!terminalized.LatestCheckpointPersisted {
			t.Fatalf("terminalization = %#v", terminalized)
		}
	})

	tests := []struct {
		name       string
		failAt     int
		wantEvents []loom.CheckpointObservationStage
		wantSeq    int64
	}{
		{
			name:       "first latest put fails",
			failAt:     1,
			wantEvents: []loom.CheckpointObservationStage{loom.CheckpointLatestPutBefore},
			wantSeq:    1,
		},
		{
			name:   "final latest put fails after prior success",
			failAt: 2,
			wantEvents: []loom.CheckpointObservationStage{
				loom.CheckpointLatestPutBefore,
				loom.CheckpointLatestPutAfter,
				loom.CheckpointLatestPutBefore,
			},
			wantSeq: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &lifecyclePutFaultStore{
				Store:  loom.NewMemStore(),
				failAt: test.failAt,
				err:    errors.New("latest checkpoint unavailable"),
			}
			graph := loom.NewGraph("lifecycle-checkpoint-failure", "first")
			if test.failAt == 1 {
				graph.AddStep("first", lifecycleNoopStep, loom.End())
			} else {
				graph.AddStep("first", lifecycleNoopStep, loom.Always("second"))
				graph.AddStep("second", lifecycleNoopStep, loom.End())
			}
			var stages []loom.CheckpointObservationStage
			var terminalized loom.TerminalizationEvent

			result, err := graph.RunWithLifecycle(
				context.Background(),
				loom.State{"__run_id": "run-checkpoint-" + test.name},
				store,
				loom.LifecycleHooks{
					CheckpointObserver: lifecycleCheckpointObserverFunc(func(
						_ context.Context,
						event loom.CheckpointEvent,
					) error {
						stages = append(stages, event.Stage)
						return nil
					}),
					Terminalizer: lifecycleTerminalizerFunc(func(
						_ context.Context,
						event loom.TerminalizationEvent,
					) error {
						terminalized = event
						return nil
					}),
				},
			)
			if err != nil || result == nil || result.StopReason != loom.StopCompleted {
				t.Fatalf("result=%#v error=%v", result, err)
			}
			if !reflect.DeepEqual(stages, test.wantEvents) {
				t.Fatalf("checkpoint stages = %#v, want %#v", stages, test.wantEvents)
			}
			if terminalized.LatestCheckpointSeq != test.wantSeq ||
				terminalized.LatestCheckpointPersisted {
				t.Fatalf("terminalization = %#v", terminalized)
			}
		})
	}

	t.Run("observer error follows graph error path", func(t *testing.T) {
		observerErr := errors.New("checkpoint observer failed")
		graph := loom.NewGraph("lifecycle-checkpoint-observer-error", "work")
		graph.AddStep("work", lifecycleNoopStep, loom.End())
		terminalizerCalls := 0
		var terminalized loom.TerminalizationEvent

		result, err := graph.RunWithLifecycle(
			context.Background(),
			loom.State{"__run_id": "run-checkpoint-observer-error"},
			loom.NewMemStore(),
			loom.LifecycleHooks{
				CheckpointObserver: lifecycleCheckpointObserverFunc(func(
					_ context.Context,
					event loom.CheckpointEvent,
				) error {
					if event.Stage != loom.CheckpointLatestPutBefore {
						t.Fatalf("unexpected observer event = %#v", event)
					}
					return observerErr
				}),
				Terminalizer: lifecycleTerminalizerFunc(func(
					_ context.Context,
					event loom.TerminalizationEvent,
				) error {
					terminalizerCalls++
					terminalized = event
					return nil
				}),
			},
		)
		if result == nil || result.StopReason != loom.StopError {
			t.Fatalf("result = %#v", result)
		}
		if !errors.Is(err, observerErr) {
			t.Fatalf("error = %v, want observer sentinel", err)
		}
		if terminalizerCalls != 1 ||
			terminalized.LatestCheckpointSeq != 1 ||
			terminalized.LatestCheckpointPersisted {
			t.Fatalf(
				"terminalizer calls=%d event=%#v",
				terminalizerCalls,
				terminalized,
			)
		}
	})
}

func TestResumeAtWithLifecycleUsesCallerIdentityBeforeNoStepRoute(t *testing.T) {
	type executionContextKey struct{}
	store := loom.NewMemStore()
	var order []string
	recordOrder := false
	stepCalls := 0
	graph := loom.NewGraph(
		"lifecycle-fork",
		"source",
		loom.WithCheckpointHistory(-1),
	)
	graph.AddStep("source", func(context.Context, loom.State) (loom.State, error) {
		stepCalls++
		return loom.State{"source_done": true}, nil
	}, func(ctx context.Context, _ loom.State) (string, error) {
		if recordOrder {
			order = append(order, "route")
			if active, _ := ctx.Value(executionContextKey{}).(bool); !active {
				return "", errors.New("fork route did not receive execution context")
			}
		}
		return "", nil
	})
	source, err := graph.Run(context.Background(), loom.State{}, store)
	if err != nil {
		t.Fatalf("seed source: %v", err)
	}
	recordOrder = true
	var allocated loom.RunAllocationEvent
	var terminalized loom.TerminalizationEvent

	fork, err := graph.ResumeAtWithLifecycle(
		context.Background(),
		source.RunID,
		1,
		"run-fork-allocated",
		loom.State{"fork_input": "merged"},
		store,
		loom.LifecycleHooks{
			RunAllocated: lifecycleRunAllocatedFunc(func(
				_ context.Context,
				event loom.RunAllocationEvent,
			) error {
				order = append(order, "allocate")
				allocated = event
				event.State["hook_mutation"] = true
				return nil
			}),
			ExecutionContext: lifecycleExecutionContextFunc(func(
				ctx context.Context,
				event loom.RunAllocationEvent,
			) (context.Context, error) {
				order = append(order, "context")
				if event.RunID != allocated.RunID {
					return nil, errors.New("execution context preceded allocation")
				}
				return context.WithValue(
					ctx,
					executionContextKey{},
					true,
				), nil
			}),
			Terminalizer: lifecycleTerminalizerFunc(func(
				ctx context.Context,
				event loom.TerminalizationEvent,
			) error {
				order = append(order, "terminalize")
				if active, _ := ctx.Value(executionContextKey{}).(bool); active {
					return errors.New(
						"terminalizer received execution-only context",
					)
				}
				terminalized = event
				return nil
			}),
		},
	)
	if err != nil {
		t.Fatalf("ResumeAtWithLifecycle: %v", err)
	}
	if fork == nil {
		t.Fatal("ResumeAtWithLifecycle returned nil result")
	}
	if !reflect.DeepEqual(
		order,
		[]string{"allocate", "context", "route", "terminalize"},
	) {
		t.Fatalf("callback order = %#v", order)
	}
	if stepCalls != 1 {
		t.Fatalf("step calls = %d, want source execution only", stepCalls)
	}
	if fork.RunID != "run-fork-allocated" ||
		fork.StopReason != loom.StopCompleted ||
		fork.State["fork_input"] != "merged" ||
		fork.State["hook_mutation"] != nil {
		t.Fatalf("fork result = %#v", fork)
	}
	if allocated.RunID != fork.RunID ||
		allocated.ParentRunID != source.RunID ||
		allocated.ParentSeq != 1 ||
		allocated.State["__run_id"] != fork.RunID ||
		allocated.State["__parent_run"] != source.RunID ||
		allocated.State["__parent_seq"] != int64(1) {
		t.Fatalf("allocation event = %#v", allocated)
	}
	if terminalized.RunID != fork.RunID ||
		terminalized.StopReason != loom.StopCompleted ||
		terminalized.LatestCheckpointSeq != 0 ||
		terminalized.LatestCheckpointPersisted {
		t.Fatalf("terminalization = %#v", terminalized)
	}
}

func TestLifecycleExecutionContextFailureStopsBeforeGraph(t *testing.T) {
	executionContextErr := errors.New("execution context unavailable")
	for _, test := range []struct {
		name string
		hook lifecycleExecutionContextFunc
		want error
	}{
		{
			name: "error",
			hook: func(
				context.Context,
				loom.RunAllocationEvent,
			) (context.Context, error) {
				return nil, executionContextErr
			},
			want: executionContextErr,
		},
		{
			name: "nil context",
			hook: func(
				context.Context,
				loom.RunAllocationEvent,
			) (context.Context, error) {
				return nil, nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			stepCalls := 0
			terminalizerCalls := 0
			graph := loom.NewGraph("lifecycle-context-failure", "work")
			graph.AddStep(
				"work",
				func(context.Context, loom.State) (loom.State, error) {
					stepCalls++
					return nil, nil
				},
				loom.End(),
			)
			result, err := graph.RunWithLifecycle(
				context.Background(),
				loom.State{"__run_id": "run-context-" + test.name},
				nil,
				loom.LifecycleHooks{
					ExecutionContext: test.hook,
					Terminalizer: lifecycleTerminalizerFunc(func(
						context.Context,
						loom.TerminalizationEvent,
					) error {
						terminalizerCalls++
						return nil
					}),
				},
			)
			if result != nil || stepCalls != 0 || terminalizerCalls != 0 {
				t.Fatalf(
					"result=%#v step calls=%d terminalizer calls=%d",
					result,
					stepCalls,
					terminalizerCalls,
				)
			}
			if test.want != nil {
				if !errors.Is(err, test.want) {
					t.Fatalf("error = %v, want %v", err, test.want)
				}
			} else if err == nil ||
				!strings.Contains(err.Error(), "nil context") {
				t.Fatalf("error = %v, want nil-context failure", err)
			}
		})
	}
}

func TestResumeAtWithLifecycleExecutionContextReachesNextStep(t *testing.T) {
	store := loom.NewMemStore()
	graph := loom.NewGraph(
		"lifecycle-fork-execution-context",
		"source",
		loom.WithCheckpointHistory(-1),
	)
	graph.AddStep(
		"source",
		func(context.Context, loom.State) (loom.State, error) {
			return loom.State{
				"__yield":       true,
				"__yield_phase": "after_step",
			}, nil
		},
		loom.Always("next"),
	)
	nextCalls := 0
	graph.AddStep("next", func(ctx context.Context, _ loom.State) (loom.State, error) {
		nextCalls++
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}, loom.End())
	source, err := graph.Run(context.Background(), loom.State{}, store)
	if err != nil || source == nil || !source.Yielded {
		t.Fatalf("seed source=%#v error=%v", source, err)
	}
	executionLoss := errors.New("execution lease lost")
	terminalizerCalls := 0

	fork, err := graph.ResumeAtWithLifecycle(
		context.Background(),
		source.RunID,
		1,
		"run-fork-cancelled",
		nil,
		store,
		loom.LifecycleHooks{
			ExecutionContext: lifecycleExecutionContextFunc(func(
				ctx context.Context,
				_ loom.RunAllocationEvent,
			) (context.Context, error) {
				executionCtx, cancel := context.WithCancelCause(ctx)
				cancel(executionLoss)
				return executionCtx, nil
			}),
			Terminalizer: lifecycleTerminalizerFunc(func(
				_ context.Context,
				event loom.TerminalizationEvent,
			) error {
				terminalizerCalls++
				if event.StopReason != loom.StopError {
					t.Fatalf("terminalization = %#v", event)
				}
				return nil
			}),
		},
	)
	if fork == nil ||
		fork.StopReason != loom.StopError ||
		nextCalls != 1 ||
		terminalizerCalls != 1 ||
		!errors.Is(err, executionLoss) {
		t.Fatalf(
			"fork=%#v next calls=%d terminalizer calls=%d error=%v",
			fork,
			nextCalls,
			terminalizerCalls,
			err,
		)
	}
}

func TestSubGraphLifecycleHooksCoverFreshAndResume(t *testing.T) {
	store := loom.NewMemStore()
	child := loom.NewGraph("lifecycle-child", "work")
	child.AddStep("work", func(_ context.Context, state loom.State) (loom.State, error) {
		if _, resumed := state["__resumed_tool_results"]; resumed {
			return loom.State{"output": "done"}, nil
		}
		return lifecycleApprovalYield("call-example"), nil
	}, loom.End())

	allocations := 0
	var terminalizations []loom.TerminalizationEvent
	hooks := &loom.LifecycleHooks{
		RunAllocated: lifecycleRunAllocatedFunc(func(
			_ context.Context,
			event loom.RunAllocationEvent,
		) error {
			allocations++
			if event.RunID != "run-child-example" || event.GraphName != child.Name {
				t.Fatalf("allocation event = %#v", event)
			}
			return nil
		}),
		Terminalizer: lifecycleTerminalizerFunc(func(
			_ context.Context,
			event loom.TerminalizationEvent,
		) error {
			terminalizations = append(terminalizations, event)
			return nil
		}),
	}
	step := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{
		LifecycleHooks: hooks,
	})
	parent := loom.State{"__run_id": "run-child-example", "parent": "kept"}

	parked, err := step(context.Background(), parent)
	if err != nil {
		t.Fatalf("fresh child: %v", err)
	}
	if allocations != 1 ||
		len(terminalizations) != 1 ||
		!terminalizations[0].Yielded ||
		terminalizations[0].StopReason != loom.StopYielded {
		t.Fatalf(
			"allocations=%d terminalizations=%#v",
			allocations,
			terminalizations,
		)
	}
	resumeState := parent.Merge(parked, nil)
	resumeState["__child_resume_input"] = map[string]any{
		"__resumed_tool_results": map[string]any{
			"call-example": map[string]any{"content": "approved", "is_error": false},
		},
	}

	completed, err := step(context.Background(), resumeState)
	if err != nil {
		t.Fatalf("resume child: %v", err)
	}
	if allocations != 1 {
		t.Fatalf("allocation calls = %d, want fresh only", allocations)
	}
	if len(terminalizations) != 2 ||
		terminalizations[1].Yielded ||
		terminalizations[1].StopReason != loom.StopCompleted ||
		terminalizations[1].RunID != terminalizations[0].RunID {
		t.Fatalf("terminalizations = %#v", terminalizations)
	}
	if completed["output"] != "done" {
		t.Fatalf("completed delta = %#v", completed)
	}
}

func lifecycleNoopStep(context.Context, loom.State) (loom.State, error) {
	return loom.State{"done": true}, nil
}

func lifecycleApprovalYield(callID string) loom.State {
	return loom.State{
		"__yield":       true,
		"__yield_phase": "mid_step",
		"yield_type":    "await_approval",
		"__toolloop_pending": []map[string]any{{
			"park_ref": "park-example",
			"call_id":  callID,
			"tool":     "example_write",
			"args":     `{"part":"PART-0001"}`,
		}},
	}
}
