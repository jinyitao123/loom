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
