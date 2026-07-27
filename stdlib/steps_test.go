package stdlib_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/stdlib"
)

type identityCompressor struct{}

func (identityCompressor) Compress(msgs []interface{}) []interface{} { return msgs }

func TestChildContinuationProtocolMatrix(t *testing.T) {
	tests := []struct {
		name       string
		state      func(loom.State) loom.State
		wantRun    int
		wantResume int
		wantErr    string
	}{
		{name: "fresh run", state: func(loom.State) loom.State { return loom.State{} }, wantRun: 1},
		{name: "parked without input", state: func(continuation loom.State) loom.State {
			return continuation
		}, wantErr: "resume input"},
		{name: "input without continuation", state: func(loom.State) loom.State {
			return validChildResumeState(loom.State{})
		}, wantErr: "continuation"},
		{name: "partial continuation with input", state: func(loom.State) loom.State {
			return validChildResumeState(loom.State{"__child_run_id": "partial"})
		}, wantErr: "continuation"},
		{name: "complete continuation with input", state: func(continuation loom.State) loom.State {
			return validChildResumeState(continuation)
		}, wantResume: 1},
	}

	constructors := []struct {
		name string
		make func(*loom.Graph, loom.Store) loom.Step
	}{
		{name: "subgraph", make: func(child *loom.Graph, store loom.Store) loom.Step {
			return stdlib.NewSubGraphStep(child, store)
		}},
		{name: "handoff", make: func(child *loom.Graph, store loom.Store) loom.Step {
			return stdlib.NewHandoffStep(child, store, identityCompressor{})
		}},
	}

	for _, constructor := range constructors {
		for _, tt := range tests {
			t.Run(constructor.name+"/"+tt.name, func(t *testing.T) {
				store := loom.NewMemStore()
				runs := 0
				resumes := 0
				child := continuationTestChild("child", &runs, &resumes, nil)
				step := constructor.make(child, store)
				parked, err := step(context.Background(), loom.State{})
				assertNoError(t, err)
				runs = 0

				result, err := step(context.Background(), tt.state(loom.State{}.Merge(parked, nil)))
				if tt.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
						t.Fatalf("error = %v, want protocol error containing %q", err, tt.wantErr)
					}
				} else {
					assertNoError(t, err)
					if result == nil {
						t.Fatal("result is nil")
					}
				}
				if runs != tt.wantRun || resumes != tt.wantResume {
					t.Fatalf("calls Run=%d Resume=%d, want Run=%d Resume=%d", runs, resumes, tt.wantRun, tt.wantResume)
				}
			})
		}
	}
}

func TestChildContinuationResumeValidationEnvelopeAndCleanup(t *testing.T) {
	store := loom.NewMemStore()
	runs, resumes := 0, 0
	child := continuationTestChild("approval-child", &runs, &resumes, nil)
	step := stdlib.NewSubGraphStep(child, store)

	first, err := step(context.Background(), loom.State{})
	assertNoError(t, err)
	if runs != 1 || resumes != 0 {
		t.Fatalf("initial calls Run=%d Resume=%d", runs, resumes)
	}
	if got := first["yield_type"]; got != "await_approval" {
		t.Fatalf("yield_type = %v", got)
	}
	pending, ok := first["__child_pending"].([]map[string]string)
	if !ok || len(pending) != 1 {
		t.Fatalf("pending = %#v", first["__child_pending"])
	}
	if !reflect.DeepEqual(pending[0], map[string]string{"park_ref": "park-1", "call_id": "call-1", "tool": "write"}) {
		t.Fatalf("pending envelope = %#v", pending)
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(toJSON(t, first["__child_pending"]))), "args") {
		t.Fatalf("pending envelope leaked args: %#v", pending)
	}

	resumeState := loom.State{}.Merge(first, nil)
	resumeState = validChildResumeState(resumeState)
	done, err := step(context.Background(), resumeState)
	assertNoError(t, err)
	if runs != 1 || resumes != 1 {
		t.Fatalf("calls Run=%d Resume=%d, want 1/1", runs, resumes)
	}
	wantDeleted := []string{
		"__child_run_id", "__child_graph", "__child_graph_revision", "__child_yield_token",
		"__child_checkpoint_seq", "__child_resume_input", "__child_pending",
	}
	if got := done["__deleted_keys"]; !reflect.DeepEqual(got, wantDeleted) {
		t.Fatalf("deleted keys = %#v, want %#v", got, wantDeleted)
	}
}

func TestChildContinuationRejectsMalformedResumeInputAndUnknownCall(t *testing.T) {
	tests := []struct {
		name  string
		input any
	}{
		{name: "extra top-level key", input: map[string]any{"__resumed_tool_results": map[string]any{"call-1": map[string]any{"content": "ok", "is_error": false}}, "extra": true}},
		{name: "extra result field", input: map[string]any{"__resumed_tool_results": map[string]any{"call-1": map[string]any{"content": "ok", "is_error": false, "args": "secret"}}}},
		{name: "unknown call", input: map[string]any{"__resumed_tool_results": map[string]any{"other": map[string]any{"content": "ok", "is_error": false}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := loom.NewMemStore()
			child := continuationTestChild("validation-child", new(int), new(int), nil)
			parked, err := child.Run(context.Background(), loom.State{}, store)
			assertNoError(t, err)
			step := stdlib.NewSubGraphStep(child, store)
			_, err = step(context.Background(), loom.State{
				"__child_run_id": parked.RunID, "__child_graph": child.Name, "__child_resume_input": tt.input,
			})
			if err == nil {
				t.Fatal("expected protocol error")
			}
		})
	}
}

func TestChildContinuationNonApprovalYieldHasNoEnvelopeAndResumeYieldRefreshes(t *testing.T) {
	store := loom.NewMemStore()
	runs, resumes := 0, 0
	child := continuationTestChild("repeat-child", &runs, &resumes, func(resume int) bool { return resume == 1 })
	step := stdlib.NewSubGraphStep(child, store)
	first, err := step(context.Background(), loom.State{"yield_type_override": "other"})
	assertNoError(t, err)
	if _, ok := first["__child_pending"]; ok {
		t.Fatalf("non-approval yield has envelope: %#v", first)
	}

	// Use an approval child to exercise a second yield after Resume.
	child = continuationTestChild("repeat-approval-child", &runs, &resumes, func(resume int) bool { return resume == 1 })
	step = stdlib.NewSubGraphStep(child, store)
	first, err = step(context.Background(), loom.State{})
	assertNoError(t, err)
	state := validChildResumeState(loom.State{}.Merge(first, nil))
	again, err := step(context.Background(), state)
	assertNoError(t, err)
	if again["__child_run_id"] != first["__child_run_id"] {
		t.Fatalf("run ID changed: first=%v second=%v", first["__child_run_id"], again["__child_run_id"])
	}
	firstToken, firstTokenOK := first["__child_yield_token"].(string)
	againToken, againTokenOK := again["__child_yield_token"].(string)
	if !firstTokenOK || !againTokenOK || firstToken == "" || againToken == "" || firstToken == againToken {
		t.Fatalf("yield tokens first=%#v second=%#v, want distinct non-empty strings", first["__child_yield_token"], again["__child_yield_token"])
	}
	if first["__child_checkpoint_seq"] == again["__child_checkpoint_seq"] {
		t.Fatalf("checkpoint seq did not refresh: first=%#v second=%#v", first["__child_checkpoint_seq"], again["__child_checkpoint_seq"])
	}
	if first["__child_graph_revision"] != again["__child_graph_revision"] {
		t.Fatalf("graph revision changed without topology change: first=%#v second=%#v", first["__child_graph_revision"], again["__child_graph_revision"])
	}
	if deleted, _ := again["__deleted_keys"].([]string); !reflect.DeepEqual(deleted, []string{"__child_resume_input"}) {
		t.Fatalf("resume input cleanup = %#v", again["__deleted_keys"])
	}
}

func TestChildContinuationRejectsStaleGenerationAndTopologyWithoutExecutingChild(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(loom.State, *loom.Graph)
	}{
		{name: "token", mutate: func(state loom.State, _ *loom.Graph) { state["__child_yield_token"] = "stale" }},
		{name: "seq", mutate: func(state loom.State, _ *loom.Graph) { state["__child_checkpoint_seq"] = int64(999) }},
		{name: "topology revision", mutate: func(_ loom.State, child *loom.Graph) {
			child.AddStep("changed", func(context.Context, loom.State) (loom.State, error) {
				return loom.State{}, nil
			}, loom.End())
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := loom.NewMemStore()
			runs, resumes := 0, 0
			child := continuationTestChild("stale-child", &runs, &resumes, nil)
			step := stdlib.NewSubGraphStep(child, store)
			parked, err := step(context.Background(), loom.State{})
			assertNoError(t, err)
			state := validChildResumeState(loom.State{}.Merge(parked, nil))
			tt.mutate(state, child)

			_, err = step(context.Background(), state)
			if err == nil || !strings.Contains(err.Error(), "stale continuation") {
				t.Fatalf("error = %v, want stale continuation protocol error", err)
			}
			if runs != 1 || resumes != 0 {
				t.Fatalf("calls Run=%d Resume=%d, want 1/0", runs, resumes)
			}
		})
	}
}

func TestChildContinuationResumesLegacyEmptyYieldPhaseCheckpoint(t *testing.T) {
	store := loom.NewMemStore()
	runs, resumes := 0, 0
	child := continuationTestChild("legacy-phase-child", &runs, &resumes, nil)
	step := stdlib.NewSubGraphStep(child, store)
	parked, err := step(context.Background(), loom.State{})
	assertNoError(t, err)

	runID := parked["__child_run_id"].(string)
	data, err := store.Get(context.Background(), "checkpoint:"+child.Name, runID)
	assertNoError(t, err)
	var checkpoint map[string]any
	assertNoError(t, json.Unmarshal(data, &checkpoint))
	checkpoint["yield_phase"] = ""
	data, err = json.Marshal(checkpoint)
	assertNoError(t, err)
	assertNoError(t, store.Put(context.Background(), "checkpoint:"+child.Name, runID, data))

	done, err := step(context.Background(), validChildResumeState(loom.State{}.Merge(parked, nil)))
	if err != nil {
		t.Errorf("resume error = %v, want nil", err)
	}
	if runs != 1 || resumes != 1 {
		t.Errorf("calls Run=%d Resume=%d, want 1/1", runs, resumes)
	}
	if done == nil {
		return
	}
	if got := done["output"]; got != "done" {
		t.Fatalf("output = %v, want done", got)
	}
	wantDeleted := []string{
		"__child_run_id", "__child_graph", "__child_graph_revision", "__child_yield_token",
		"__child_checkpoint_seq", "__child_resume_input", "__child_pending",
	}
	if got := done["__deleted_keys"]; !reflect.DeepEqual(got, wantDeleted) {
		t.Fatalf("deleted keys = %#v, want %#v", got, wantDeleted)
	}
}

func TestChildContinuationRejectsLatestCheckpointThatIsNoLongerYielded(t *testing.T) {
	store := loom.NewMemStore()
	runs, resumes := 0, 0
	child := continuationTestChild("completed-child", &runs, &resumes, nil)
	step := stdlib.NewSubGraphStep(child, store)
	parked, err := step(context.Background(), loom.State{})
	assertNoError(t, err)

	runID := parked["__child_run_id"].(string)
	data, err := store.Get(context.Background(), "checkpoint:"+child.Name, runID)
	assertNoError(t, err)
	var checkpoint map[string]any
	assertNoError(t, json.Unmarshal(data, &checkpoint))
	checkpoint["yield_phase"] = ""
	checkpoint["state"].(map[string]any)["__yield"] = false
	data, err = json.Marshal(checkpoint)
	assertNoError(t, err)
	assertNoError(t, store.Put(context.Background(), "checkpoint:"+child.Name, runID, data))

	_, err = step(context.Background(), validChildResumeState(loom.State{}.Merge(parked, nil)))
	if err == nil || !strings.Contains(err.Error(), "yielded checkpoint") {
		t.Fatalf("error = %v, want non-yielded checkpoint protocol error", err)
	}
	if runs != 1 || resumes != 0 {
		t.Fatalf("calls Run=%d Resume=%d, want 1/0", runs, resumes)
	}
}

func TestChildContinuationRejectsChangedEntryWithoutExecutingReplacement(t *testing.T) {
	store := loom.NewMemStore()
	originalRuns, originalResumes := 0, 0
	original := continuationTestChild("entry-child", &originalRuns, &originalResumes, nil)
	parked, err := stdlib.NewSubGraphStep(original, store)(context.Background(), loom.State{})
	assertNoError(t, err)

	replacementRuns := 0
	replacement := loom.NewGraph("entry-child", "other")
	replacement.AddStep("work", func(context.Context, loom.State) (loom.State, error) {
		replacementRuns++
		return loom.State{}, nil
	}, loom.End())
	replacement.AddStep("other", func(context.Context, loom.State) (loom.State, error) {
		replacementRuns++
		return loom.State{}, nil
	}, loom.End())

	state := validChildResumeState(loom.State{}.Merge(parked, nil))
	_, err = stdlib.NewSubGraphStep(replacement, store)(context.Background(), state)
	if err == nil || !strings.Contains(err.Error(), "stale continuation") {
		t.Fatalf("error = %v, want stale continuation protocol error", err)
	}
	if replacementRuns != 0 {
		t.Fatalf("replacement child executed %d steps, want 0", replacementRuns)
	}
}

func TestChildContinuationPreservesWrappedChildError(t *testing.T) {
	want := errors.New("child exploded")
	store := loom.NewMemStore()
	child := loom.NewGraph("error-child", "fail")
	child.AddStep("fail", func(context.Context, loom.State) (loom.State, error) { return nil, want }, loom.End())
	step := stdlib.NewSubGraphStep(child, store)
	_, err := step(context.Background(), loom.State{})
	if !errors.Is(err, want) || !strings.Contains(err.Error(), child.Name) {
		t.Fatalf("error = %v, want wrapped child error", err)
	}
}

func TestSubGraphChildObserverLifecycle(t *testing.T) {
	t.Run("fresh_success", func(t *testing.T) {
		store := loom.NewMemStore()
		child := loom.NewGraph("observer-success-child", "work")
		child.AddStep("work", func(context.Context, loom.State) (loom.State, error) {
			return loom.State{"child_only": "visible"}, nil
		}, loom.End())

		calls := 0
		var observer stdlib.ChildLifecycleObserver = func(_ context.Context, result *loom.RunResult, runErr error, resumed bool) error {
			calls++
			if resumed {
				t.Fatal("resumed = true, want false")
			}
			if runErr != nil {
				t.Fatalf("runErr = %v, want nil", runErr)
			}
			if result.RunID == "" {
				t.Fatal("RunID is empty")
			}
			if result.StopReason != loom.StopCompleted {
				t.Fatalf("StopReason = %q, want %q", result.StopReason, loom.StopCompleted)
			}
			if got := result.State["child_only"]; got != "visible" {
				t.Fatalf("child state = %v, want visible", got)
			}
			return nil
		}
		step := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{ChildObserver: observer})

		delta, err := step(context.Background(), loom.State{"unchanged": "parent"})
		assertNoError(t, err)
		if calls != 1 {
			t.Fatalf("observer calls = %d, want 1", calls)
		}
		if !reflect.DeepEqual(delta, loom.State{"__seq": int64(1), "child_only": "visible"}) {
			t.Fatalf("delta = %#v, want original child delta", delta)
		}
	})

	t.Run("fresh_yield", func(t *testing.T) {
		store := loom.NewMemStore()
		child := observerYieldChild("observer-yield-child", false)
		calls := 0
		step := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{
			ChildObserver: func(_ context.Context, result *loom.RunResult, runErr error, resumed bool) error {
				calls++
				if resumed || runErr != nil {
					t.Fatalf("resumed=%v runErr=%v, want false/nil", resumed, runErr)
				}
				if !result.Yielded || result.StopReason != loom.StopYielded {
					t.Fatalf("yielded=%v StopReason=%q, want true/%q", result.Yielded, result.StopReason, loom.StopYielded)
				}
				assertObserverRawChildState(t, result)
				return nil
			},
		})

		delta, err := step(context.Background(), loom.State{})
		assertNoError(t, err)
		if calls != 1 {
			t.Fatalf("observer calls = %d, want 1", calls)
		}
		assertSanitizedChildYield(t, delta)
	})

	t.Run("fresh_partial_error", func(t *testing.T) {
		childErr := errors.New("child partial failure")
		store := loom.NewMemStore()
		child := loom.NewGraph("observer-error-child", "fail")
		child.AddStep("fail", func(context.Context, loom.State) (loom.State, error) {
			return loom.State{"ignored": "delta"}, childErr
		}, loom.End())
		calls := 0
		step := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{
			ChildObserver: func(_ context.Context, result *loom.RunResult, runErr error, resumed bool) error {
				calls++
				if resumed || result == nil {
					t.Fatalf("resumed=%v result=%v, want false/non-nil", resumed, result)
				}
				if !errors.Is(runErr, childErr) {
					t.Fatalf("runErr = %v, want child sentinel", runErr)
				}
				if result.StopReason != loom.StopError || result.State["__failed_step"] != "fail" {
					t.Fatalf("partial result = %#v", result)
				}
				return nil
			},
		})

		state, err := step(context.Background(), loom.State{"input": "kept"})
		if !errors.Is(err, childErr) {
			t.Fatalf("error = %v, want child sentinel", err)
		}
		if calls != 1 {
			t.Fatalf("observer calls = %d, want 1", calls)
		}
		if state == nil || state["input"] != "kept" || state["__failed_step"] != "fail" {
			t.Fatalf("partial state = %#v", state)
		}
	})

	t.Run("resume_success", func(t *testing.T) {
		store := loom.NewMemStore()
		runs, resumes := 0, 0
		child := continuationTestChild("observer-resume-child", &runs, &resumes, nil)
		type observation struct {
			runID   string
			resumed bool
			yielded bool
		}
		var observations []observation
		step := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{
			ChildObserver: func(_ context.Context, result *loom.RunResult, runErr error, resumed bool) error {
				if runErr != nil {
					t.Fatalf("runErr = %v, want nil", runErr)
				}
				observations = append(observations, observation{
					runID: result.RunID, resumed: resumed, yielded: result.Yielded,
				})
				return nil
			},
		})

		first, err := step(context.Background(), loom.State{})
		assertNoError(t, err)
		done, err := step(context.Background(), validChildResumeState(loom.State{}.Merge(first, nil)))
		assertNoError(t, err)
		if len(observations) != 2 {
			t.Fatalf("observer calls = %d, want 2", len(observations))
		}
		if observations[0].resumed || !observations[0].yielded {
			t.Fatalf("fresh observation = %#v", observations[0])
		}
		if !observations[1].resumed || observations[1].yielded {
			t.Fatalf("resume observation = %#v", observations[1])
		}
		if observations[0].runID == "" || observations[1].runID != observations[0].runID {
			t.Fatalf("run IDs = %#v, want same non-empty ID", observations)
		}
		assertChildContinuationCleanup(t, done)
	})

	t.Run("resume_yield", func(t *testing.T) {
		store := loom.NewMemStore()
		runs, resumes := 0, 0
		child := continuationTestChild("observer-resume-yield-child", &runs, &resumes, func(resume int) bool {
			return resume == 1
		})
		var runIDs []string
		var resumedValues []bool
		step := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{
			ChildObserver: func(_ context.Context, result *loom.RunResult, runErr error, resumed bool) error {
				if runErr != nil || !result.Yielded {
					t.Fatalf("runErr=%v yielded=%v, want nil/true", runErr, result.Yielded)
				}
				runIDs = append(runIDs, result.RunID)
				resumedValues = append(resumedValues, resumed)
				return nil
			},
		})

		first, err := step(context.Background(), loom.State{})
		assertNoError(t, err)
		again, err := step(context.Background(), validChildResumeState(loom.State{}.Merge(first, nil)))
		assertNoError(t, err)
		if len(runIDs) != 2 || resumedValues[0] || !resumedValues[1] {
			t.Fatalf("observations runIDs=%#v resumed=%#v", runIDs, resumedValues)
		}
		if runIDs[0] == "" || runIDs[1] != runIDs[0] {
			t.Fatalf("run IDs = %#v, want same non-empty ID", runIDs)
		}
		if first["__child_yield_token"] == again["__child_yield_token"] {
			t.Fatalf("yield token did not refresh: first=%v second=%v", first["__child_yield_token"], again["__child_yield_token"])
		}
		if first["__child_checkpoint_seq"] == again["__child_checkpoint_seq"] {
			t.Fatalf("checkpoint seq did not refresh: first=%v second=%v", first["__child_checkpoint_seq"], again["__child_checkpoint_seq"])
		}
	})

	t.Run("validation_error_without_result", func(t *testing.T) {
		store := loom.NewMemStore()
		runs, resumes := 0, 0
		child := continuationTestChild("observer-validation-child", &runs, &resumes, nil)
		calls := 0
		step := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{
			ChildObserver: func(context.Context, *loom.RunResult, error, bool) error {
				calls++
				return nil
			},
		})
		first, err := step(context.Background(), loom.State{})
		assertNoError(t, err)
		calls = 0
		state := validChildResumeState(loom.State{}.Merge(first, nil))
		state["__child_yield_token"] = "stale"

		result, err := step(context.Background(), state)
		if err == nil || !strings.Contains(err.Error(), "stale continuation") {
			t.Fatalf("error = %v, want stale continuation error", err)
		}
		if result != nil {
			t.Fatalf("result = %#v, want nil", result)
		}
		if calls != 0 || runs != 1 || resumes != 0 {
			t.Fatalf("observer=%d Run=%d Resume=%d, want 0/1/0", calls, runs, resumes)
		}
	})
}

func TestSubGraphChildObserverRunsBeforeProjection(t *testing.T) {
	t.Run("yield_bubble", func(t *testing.T) {
		store := loom.NewMemStore()
		child := observerYieldChild("observer-bubble-child", false)
		step := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{
			ChildObserver: func(_ context.Context, result *loom.RunResult, _ error, _ bool) error {
				assertObserverRawChildState(t, result)
				return nil
			},
		})

		delta, err := step(context.Background(), loom.State{})
		assertNoError(t, err)
		assertSanitizedChildYield(t, delta)
	})

	t.Run("yield_trap", func(t *testing.T) {
		store := loom.NewMemStore()
		child := observerYieldChild("observer-trap-child", false)
		observed := false
		step := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{
			YieldPolicy: stdlib.YieldTrap,
			ChildObserver: func(context.Context, *loom.RunResult, error, bool) error {
				observed = true
				return nil
			},
		})

		state, err := step(context.Background(), loom.State{})
		if !observed {
			t.Fatal("observer was not called before YieldTrap")
		}
		if state != nil || err == nil || !strings.Contains(err.Error(), "YieldTrap") {
			t.Fatalf("state=%#v error=%v, want nil trap error", state, err)
		}
	})

	t.Run("yield_custom", func(t *testing.T) {
		store := loom.NewMemStore()
		child := observerYieldChild("observer-custom-child", false)
		observed := false
		handlerCalls := 0
		step := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{
			YieldPolicy: stdlib.YieldCustom,
			ChildObserver: func(context.Context, *loom.RunResult, error, bool) error {
				observed = true
				return nil
			},
			YieldHandler: func(context.Context, *loom.RunResult) (loom.State, error) {
				handlerCalls++
				if !observed {
					t.Fatal("YieldCustom handler ran before observer")
				}
				return loom.State{"handled": true}, nil
			},
		})

		state, err := step(context.Background(), loom.State{})
		assertNoError(t, err)
		if handlerCalls != 1 || !reflect.DeepEqual(state, loom.State{"handled": true}) {
			t.Fatalf("handler calls=%d state=%#v", handlerCalls, state)
		}
	})

	t.Run("success_delta", func(t *testing.T) {
		store := loom.NewMemStore()
		child := loom.NewGraph("observer-delta-child", "work")
		child.AddStep("work", func(context.Context, loom.State) (loom.State, error) {
			return loom.State{"child_only": "visible"}, nil
		}, loom.End())
		observed := false
		step := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{
			ChildObserver: func(_ context.Context, result *loom.RunResult, _ error, _ bool) error {
				observed = true
				if result.State["unchanged"] != "parent" || result.State["child_only"] != "visible" {
					t.Fatalf("observer state = %#v, want complete child state", result.State)
				}
				return nil
			},
		})

		delta, err := step(context.Background(), loom.State{"unchanged": "parent"})
		assertNoError(t, err)
		if !observed {
			t.Fatal("observer was not called")
		}
		if !reflect.DeepEqual(delta, loom.State{"__seq": int64(1), "child_only": "visible"}) {
			t.Fatalf("delta = %#v, want projected change only", delta)
		}
	})
}

func TestSubGraphChildObserverFailureFailsClosed(t *testing.T) {
	observerErr := errors.New("observer failed")

	t.Run("child_success", func(t *testing.T) {
		store := loom.NewMemStore()
		child := loom.NewGraph("observer-failure-success-child", "work")
		child.AddStep("work", func(context.Context, loom.State) (loom.State, error) {
			return loom.State{"would_leak": true}, nil
		}, loom.End())
		step := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{
			ChildObserver: func(context.Context, *loom.RunResult, error, bool) error {
				return observerErr
			},
		})

		state, err := step(context.Background(), loom.State{})
		if state != nil || !errors.Is(err, observerErr) || !strings.Contains(err.Error(), child.Name) {
			t.Fatalf("state=%#v error=%v, want nil wrapped observer error", state, err)
		}
	})

	t.Run("child_partial_error", func(t *testing.T) {
		childErr := errors.New("child failed")
		store := loom.NewMemStore()
		child := loom.NewGraph("observer-failure-error-child", "fail")
		child.AddStep("fail", func(context.Context, loom.State) (loom.State, error) {
			return loom.State{"would_leak": true}, childErr
		}, loom.End())
		step := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{
			ChildObserver: func(context.Context, *loom.RunResult, error, bool) error {
				return observerErr
			},
		})

		state, err := step(context.Background(), loom.State{})
		if state != nil || !errors.Is(err, childErr) || !errors.Is(err, observerErr) {
			t.Fatalf("state=%#v error=%v, want nil joined child/observer errors", state, err)
		}
	})

	t.Run("yield_custom_handler_not_called", func(t *testing.T) {
		store := loom.NewMemStore()
		child := observerYieldChild("observer-failure-yield-child", false)
		handlerCalls := 0
		step := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{
			YieldPolicy: stdlib.YieldCustom,
			ChildObserver: func(context.Context, *loom.RunResult, error, bool) error {
				return observerErr
			},
			YieldHandler: func(context.Context, *loom.RunResult) (loom.State, error) {
				handlerCalls++
				return loom.State{"would_leak": true}, nil
			},
		})

		state, err := step(context.Background(), loom.State{})
		if state != nil || !errors.Is(err, observerErr) {
			t.Fatalf("state=%#v error=%v, want nil observer error", state, err)
		}
		if handlerCalls != 0 {
			t.Fatalf("YieldCustom handler calls = %d, want 0", handlerCalls)
		}
	})
}

func TestSubGraphChildObserverNilPreservesDefaultBehavior(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		store := loom.NewMemStore()
		child := loom.NewGraph("nil-observer-success-child", "work")
		child.AddStep("work", func(context.Context, loom.State) (loom.State, error) {
			return loom.State{"output": "done"}, nil
		}, loom.End())

		delta, err := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{
			ChildObserver: nil,
		})(context.Background(), loom.State{"unchanged": true})
		assertNoError(t, err)
		if !reflect.DeepEqual(delta, loom.State{"__seq": int64(1), "output": "done"}) {
			t.Fatalf("delta = %#v, want default success delta", delta)
		}
	})

	t.Run("yield_and_resume", func(t *testing.T) {
		store := loom.NewMemStore()
		runs, resumes := 0, 0
		child := continuationTestChild("nil-observer-resume-child", &runs, &resumes, nil)
		step := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{ChildObserver: nil})

		first, err := step(context.Background(), loom.State{})
		assertNoError(t, err)
		assertSanitizedChildYield(t, first)
		done, err := step(context.Background(), validChildResumeState(loom.State{}.Merge(first, nil)))
		assertNoError(t, err)
		if runs != 1 || resumes != 1 || done["output"] != "done" {
			t.Fatalf("Run=%d Resume=%d done=%#v", runs, resumes, done)
		}
		assertChildContinuationCleanup(t, done)
	})

	t.Run("error", func(t *testing.T) {
		childErr := errors.New("nil observer child failure")
		store := loom.NewMemStore()
		child := loom.NewGraph("nil-observer-error-child", "fail")
		child.AddStep("fail", func(context.Context, loom.State) (loom.State, error) {
			return nil, childErr
		}, loom.End())

		state, err := stdlib.NewSubGraphStep(child, store, stdlib.SubGraphOpts{
			ChildObserver: nil,
		})(context.Background(), loom.State{"input": "kept"})
		if !errors.Is(err, childErr) || state == nil || state["__failed_step"] != "fail" {
			t.Fatalf("state=%#v error=%v, want default partial error result", state, err)
		}
	})
}

func observerYieldChild(name string, yieldAgain bool) *loom.Graph {
	child := loom.NewGraph(name, "work")
	child.AddStep("work", func(_ context.Context, state loom.State) (loom.State, error) {
		if _, resumed := state["__resumed_tool_results"]; resumed && !yieldAgain {
			return loom.State{"output": "done"}, nil
		}
		update := approvalYield("call-1")
		update["child_only"] = "visible"
		return update, nil
	}, loom.End())
	return child
}

func assertObserverRawChildState(t *testing.T, result *loom.RunResult) {
	t.Helper()
	if got := result.State["child_only"]; got != "visible" {
		t.Fatalf("child-only state = %v, want visible", got)
	}
	pending, ok := result.State["__toolloop_pending"].([]map[string]any)
	if !ok || len(pending) != 1 || pending[0]["args"] != `{"secret":true}` {
		t.Fatalf("raw pending = %#v, want complete args", result.State["__toolloop_pending"])
	}
}

func assertSanitizedChildYield(t *testing.T, state loom.State) {
	t.Helper()
	if state == nil || state["__child_run_id"] == "" || state["yield_type"] != "await_approval" {
		t.Fatalf("yield state = %#v, want child continuation", state)
	}
	if _, ok := state["child_only"]; ok {
		t.Fatalf("yield state leaked child-only key: %#v", state)
	}
	if _, ok := state["__toolloop_pending"]; ok {
		t.Fatalf("yield state leaked raw pending calls: %#v", state)
	}
	if strings.Contains(toJSON(t, state["__child_pending"]), "secret") {
		t.Fatalf("yield envelope leaked pending args: %#v", state["__child_pending"])
	}
}

func assertChildContinuationCleanup(t *testing.T, state loom.State) {
	t.Helper()
	wantDeleted := []string{
		"__child_run_id", "__child_graph", "__child_graph_revision", "__child_yield_token",
		"__child_checkpoint_seq", "__child_resume_input", "__child_pending",
	}
	if got := state["__deleted_keys"]; !reflect.DeepEqual(got, wantDeleted) {
		t.Fatalf("deleted keys = %#v, want %#v", got, wantDeleted)
	}
}

func continuationTestChild(name string, runs, resumes *int, yieldAgain func(int) bool) *loom.Graph {
	child := loom.NewGraph(name, "work")
	child.AddStep("work", func(_ context.Context, state loom.State) (loom.State, error) {
		if _, resumed := state["__resumed_tool_results"]; resumed {
			*resumes++
			if yieldAgain != nil && yieldAgain(*resumes) {
				return approvalYield("call-1"), nil
			}
			return loom.State{"output": "done"}, nil
		}
		*runs++
		update := approvalYield("call-1")
		if state["yield_type_override"] == "other" {
			update["yield_type"] = "other"
		}
		return update, nil
	}, loom.End())
	return child
}

func approvalYield(callID string) loom.State {
	return loom.State{
		"__yield": true, "__yield_phase": "mid_step", "yield_type": "await_approval",
		"__toolloop_pending": []map[string]any{{"park_ref": "park-1", "call_id": callID, "tool": "write", "args": `{"secret":true}`}},
	}
}

func validChildResumeState(state loom.State) loom.State {
	state["__child_resume_input"] = map[string]any{
		"__resumed_tool_results": map[string]any{"call-1": map[string]any{"content": "approved", "is_error": false}},
	}
	return state
}

func toJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	assertNoError(t, err)
	return string(b)
}

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
