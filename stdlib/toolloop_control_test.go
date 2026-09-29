package stdlib_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

func controlGraph(name string, llm contract.LLM, tools contract.ToolDispatcher, opts stdlib.ToolLoopOpts) *loom.Graph {
	graph := loom.NewGraph(name, "chat", loom.WithMergeConfig(loom.DefaultMergeConfig()), loom.WithCheckpointPolicy(loom.CheckpointRequired))
	graph.AddStep("chat", stdlib.NewToolLoopStep(llm, tools, opts), loom.End())
	return graph
}

func controlInput() loom.State {
	return loom.State{"messages": []contract.Message{{Role: "user", Content: "do the work"}}}
}

func controlOutcome(t *testing.T, result *loom.RunResult, reason stdlib.ToolLoopStopReason, used uint64) stdlib.ToolLoopOutcome {
	t.Helper()
	outcome, present, err := stdlib.ReadToolLoopOutcome(result.State)
	if err != nil || !present || outcome.Reason != reason || outcome.TotalRoundsUsed != used {
		t.Fatalf("outcome = %#v, present %v, err %v; want %q after %d rounds", outcome, present, err, reason, used)
	}
	if reason != stdlib.ToolLoopFinalResponse && reason != stdlib.ToolLoopToolStop {
		if result.StopReason != loom.StopYielded || !result.Yielded {
			t.Fatalf("controlled pause = %#v", result)
		}
	}
	return outcome
}

func controlGrant(t *testing.T, result *loom.RunResult, id string, total uint64) stdlib.ToolLoopResumeGrant {
	t.Helper()
	outcome, _, err := stdlib.ReadToolLoopOutcome(result.State)
	if err != nil {
		t.Fatal(err)
	}
	seq := decodeStateValue[int64](t, result.State["__checkpoint_seq"])
	return stdlib.ToolLoopResumeGrant{ID: id, ExpectedRunID: result.RunID, ExpectedCheckpointSeq: seq,
		ExpectedYieldToken: result.State["__yield_token"].(string), ExpectedSlice: outcome.Slice, AuthorizedTotalRounds: total}
}

func toolRounds(t *testing.T, count int, sameArgs bool) (*parkScriptLLM, *parkToolDispatcher) {
	t.Helper()
	m := &parkScriptLLM{t: t}
	d := &parkToolDispatcher{defs: []contract.ToolDef{{Name: "work"}}, results: map[string]contract.ToolResult{}}
	for round := 1; round <= count; round++ {
		call := contract.ToolCall{ID: fmt.Sprintf("call-%d", round), Name: "work", Args: fmt.Sprintf(`{"round":%d}`, round)}
		if sameArgs {
			call.Args = `{}`
		}
		m.scripts = append(m.scripts, func(contract.ChatRequest) contract.ChatResponse {
			return contract.ChatResponse{ToolCalls: []contract.ToolCall{call}, StopReason: "tool_calls", Usage: contract.Usage{InputTokens: 2, OutputTokens: 1}}
		})
		d.results[call.ID] = contract.ToolResult{Content: "done"}
	}
	return m, d
}

func TestControlledToolLoopSlicesAndTotal(t *testing.T) {
	for _, total := range []uint64{2, 3} {
		t.Run(fmt.Sprint(total), func(t *testing.T) {
			m, d := toolRounds(t, 2, false)
			m.scripts = append(m.scripts, func(request contract.ChatRequest) contract.ChatResponse {
				if len(request.Messages) != 5 {
					t.Fatalf("continuation messages = %#v", request.Messages)
				}
				return contract.ChatResponse{Content: "finished", StopReason: "stop", Usage: contract.Usage{InputTokens: 2, OutputTokens: 1}}
			})
			opts := stdlib.ToolLoopOpts{MaxIterations: 2, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: total}}
			graph, store := controlGraph(t.Name(), m, d, opts), loom.NewMemStore()
			result, err := graph.Run(context.Background(), controlInput(), store)
			if err != nil {
				t.Fatal(err)
			}
			reason := stdlib.ToolLoopSliceLimit
			if total == 2 {
				reason = stdlib.ToolLoopTotalLimit
			}
			controlOutcome(t, result, reason, 2)
			if _, hasOutput := result.State["output"]; hasOutput || m.calls != 2 || len(d.executed) != 2 {
				t.Fatalf("pause invented output or effects: %#v", result.State)
			}
			// A fresh graph reconstructs its closure; neither that nor bare Resume grants rounds.
			graph = controlGraph(t.Name(), m, d, opts)
			result, err = graph.Resume(context.Background(), result.RunID, nil, store)
			if err != nil {
				t.Fatal(err)
			}
			controlOutcome(t, result, reason, 2)
			if m.calls != 2 || len(d.executed) != 2 {
				t.Fatal("bare Resume added effects")
			}
			input, err := stdlib.PrepareToolLoopResume(result.State, controlGrant(t, result, "grant-1", 3))
			if err != nil {
				t.Fatal(err)
			}
			result, err = graph.Resume(context.Background(), result.RunID, input, store)
			if err != nil {
				t.Fatal(err)
			}
			outcome := controlOutcome(t, result, stdlib.ToolLoopFinalResponse, 3)
			if outcome.Slice != 2 || outcome.SliceRoundsUsed != 1 || m.calls != 3 || len(d.executed) != 2 || result.State["output"] != "finished" {
				t.Fatalf("resumed outcome = %#v", outcome)
			}
			public := decodeStateValue[[]contract.Message](t, result.State["messages"])
			if len(public) != 1 || public[0].Content != "do the work" {
				t.Fatalf("private transcript leaked through append merge: %#v", public)
			}
			usage := decodeStateValue[contract.Usage](t, result.State["usage"])
			if usage.InputTokens != 6 || usage.OutputTokens != 3 {
				t.Fatalf("usage = %#v", usage)
			}
			_, err = graph.Resume(context.Background(), result.RunID, nil, store)
			if err != nil || m.calls != 3 {
				t.Fatalf("completed checkpoint reexecuted: calls %d, err %v", m.calls, err)
			}
		})
	}
}

func TestControlledToolLoopCompactionPreservesTwoToolResults(t *testing.T) {
	calls := []contract.ToolCall{
		{ID: "first", Name: "read", Args: `{}`},
		{ID: "second", Name: "read", Args: `{}`},
	}
	m := &parkScriptLLM{t: t}
	m.scripts = []func(contract.ChatRequest) contract.ChatResponse{
		func(contract.ChatRequest) contract.ChatResponse {
			return contract.ChatResponse{ToolCalls: calls, StopReason: "tool_calls"}
		},
		func(request contract.ChatRequest) contract.ChatResponse {
			messages := request.Messages
			if len(messages) != 5 || messages[2].Role != "assistant" ||
				!reflect.DeepEqual(messages[2].ToolCalls, calls) ||
				messages[3].ToolCallID != calls[0].ID || messages[4].ToolCallID != calls[1].ID {
				t.Fatalf("controlled compaction split a tool-call batch: %#v", messages)
			}
			return contract.ChatResponse{Content: "done", StopReason: "stop"}
		},
	}
	d := &parkToolDispatcher{
		defs: []contract.ToolDef{{Name: "read"}},
		results: map[string]contract.ToolResult{
			"first":  {Content: strings.Repeat("x", 13_000)},
			"second": {Content: strings.Repeat("x", 13_000)},
		},
	}
	opts := stdlib.ToolLoopOpts{
		Model: "test", SystemPrompt: "stable identity", MaxIterations: 3,
		Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 3},
		Compaction: stdlib.NewSummaryCompactionPolicy(&summaryLLM{}, stdlib.SummaryCompactionOpts{
			TokenThreshold: 6000,
		}),
	}
	graph := controlGraph(t.Name(), m, d, opts)
	result, err := graph.Run(context.Background(), controlInput(), loom.NewMemStore())
	if err != nil || result.State["output"] != "done" || m.calls != 2 {
		t.Fatalf("controlled run = %#v, calls = %d, err = %v", result, m.calls, err)
	}
}

func TestControlledCompletionCorrectionPersistsAcrossResume(t *testing.T) {
	m := &parkScriptLLM{t: t, scripts: []func(contract.ChatRequest) contract.ChatResponse{
		func(contract.ChatRequest) contract.ChatResponse {
			return contract.ChatResponse{Content: "draft", StopReason: "stop"}
		},
		func(request contract.ChatRequest) contract.ChatResponse {
			if len(request.Messages) < 3 || !strings.Contains(request.Messages[len(request.Messages)-1].Content, "missing evidence") {
				t.Fatalf("resume lost completion feedback: %#v", request.Messages)
			}
			return contract.ChatResponse{Content: "accepted", StopReason: "stop"}
		},
	}}
	verifier := stdlib.CompletionVerifierFunc(func(_ context.Context, candidate stdlib.CompletionCandidate) (stdlib.CompletionDecision, error) {
		if candidate.Content == "accepted" {
			return stdlib.CompletionDecision{Accepted: true}, nil
		}
		return stdlib.CompletionDecision{Feedback: "missing evidence"}, nil
	})
	opts := stdlib.ToolLoopOpts{
		MaxIterations:      1,
		Control:            &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 2},
		CompletionVerifier: verifier, CompletionVerifierID: "fixture-v1",
	}
	graph, store := controlGraph(t.Name(), m, &parkToolDispatcher{}, opts), loom.NewMemStore()
	paused, err := graph.Run(context.Background(), controlInput(), store)
	if err != nil {
		t.Fatal(err)
	}
	controlOutcome(t, paused, stdlib.ToolLoopSliceLimit, 1)
	delta, err := stdlib.PrepareToolLoopResume(paused.State, controlGrant(t, paused, "grant", 2))
	if err != nil {
		t.Fatal(err)
	}
	completed, err := graph.Resume(context.Background(), paused.RunID, delta, store)
	if err != nil {
		t.Fatal(err)
	}
	controlOutcome(t, completed, stdlib.ToolLoopFinalResponse, 2)
	if completed.State["output"] != "accepted" {
		t.Fatalf("corrected output = %#v", completed.State["output"])
	}
}

func TestControlledCompletionVerifierRequiresStablePolicyID(t *testing.T) {
	verifier := stdlib.CompletionVerifierFunc(func(context.Context, stdlib.CompletionCandidate) (stdlib.CompletionDecision, error) {
		return stdlib.CompletionDecision{Accepted: true}, nil
	})
	for name, opts := range map[string]stdlib.ToolLoopOpts{
		"missing id":       {MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 1}, CompletionVerifier: verifier},
		"missing verifier": {MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 1}, CompletionVerifierID: "fixture-v1"},
	} {
		t.Run(name, func(t *testing.T) {
			model := &parkScriptLLM{t: t}
			result, err := controlGraph(t.Name(), model, &parkToolDispatcher{}, opts).Run(context.Background(), controlInput(), loom.NewMemStore())
			if err == nil || result.StopReason != loom.StopError || model.calls != 0 {
				t.Fatalf("invalid verifier policy reached model: result=%#v calls=%d err=%v", result, model.calls, err)
			}
		})
	}
}

func TestControlledToolLoopGrantBoundary(t *testing.T) {
	m, d := toolRounds(t, 2, false)
	graph, store := controlGraph(t.Name(), m, d, stdlib.ToolLoopOpts{MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 4}}), loom.NewMemStore()
	result, err := graph.Run(context.Background(), controlInput(), store)
	if err != nil {
		t.Fatal(err)
	}
	grant := controlGrant(t, result, "grant-1", 4)
	for _, mutate := range []func(*stdlib.ToolLoopResumeGrant){
		func(g *stdlib.ToolLoopResumeGrant) { g.ExpectedYieldToken = "old" },
		func(g *stdlib.ToolLoopResumeGrant) { g.ExpectedCheckpointSeq-- },
		func(g *stdlib.ToolLoopResumeGrant) { g.ExpectedRunID = "other" },
		func(g *stdlib.ToolLoopResumeGrant) { g.ExpectedSlice++ },
		func(g *stdlib.ToolLoopResumeGrant) { g.AuthorizedTotalRounds = 3 },
		func(g *stdlib.ToolLoopResumeGrant) { g.ID = "" },
	} {
		bad := grant
		mutate(&bad)
		if _, err := stdlib.PrepareToolLoopResume(result.State, bad); err == nil {
			t.Fatalf("accepted invalid grant %#v", bad)
		}
	}
	delta, err := stdlib.PrepareToolLoopResume(result.State, grant)
	if err != nil {
		t.Fatal(err)
	}
	result, err = graph.Resume(context.Background(), result.RunID, delta, store)
	if err != nil {
		t.Fatal(err)
	}
	controlOutcome(t, result, stdlib.ToolLoopSliceLimit, 2)
	if _, err := stdlib.PrepareToolLoopResume(result.State, grant); err == nil {
		t.Fatal("accepted old boundary")
	}
	if _, err := stdlib.PrepareToolLoopResume(result.State, controlGrant(t, result, grant.ID, 4)); err == nil {
		t.Fatal("reused the last grant ID")
	}
	if m.calls != 2 || len(d.executed) != 2 {
		t.Fatal("grant validation performed effects")
	}
}

func TestControlledToolLoopProviderStops(t *testing.T) {
	for _, calls := range []bool{false, true} {
		for _, stop := range []string{"length", "content_filter"} {
			t.Run(fmt.Sprintf("%s/%v", stop, calls), func(t *testing.T) {
				response := contract.ChatResponse{Content: "partial", StopReason: stop}
				if calls {
					response.ToolCalls = []contract.ToolCall{{ID: "unused", Name: "work", Args: `{}`}}
				}
				m := &parkScriptLLM{t: t, scripts: []func(contract.ChatRequest) contract.ChatResponse{func(contract.ChatRequest) contract.ChatResponse { return response }}}
				d := &parkToolDispatcher{}
				graph, store := controlGraph(t.Name(), m, d, stdlib.ToolLoopOpts{Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 4}}), loom.NewMemStore()
				result, err := graph.Run(context.Background(), controlInput(), store)
				if err != nil {
					t.Fatal(err)
				}
				reason := stdlib.ToolLoopProviderLength
				if stop != "length" {
					reason = stdlib.ToolLoopProviderStop
				}
				outcome := controlOutcome(t, result, reason, 1)
				if outcome.PartialText != "partial" || outcome.ProviderStopReason != stop || len(d.dispatched) != 0 || len(messagesFromState(t, result.State)) != 1 {
					t.Fatalf("halted response entered tool history: %#v", result.State)
				}
				if _, err := stdlib.PrepareToolLoopResume(result.State, controlGrant(t, result, "new", 5)); err == nil {
					t.Fatal("provider stop accepted a round grant")
				}
				_, err = graph.Resume(context.Background(), result.RunID, nil, store)
				if err != nil || m.calls != 1 || len(d.dispatched) != 0 {
					t.Fatalf("halted response resumed: %v", err)
				}
			})
		}
	}
}

func TestControlledToolLoopRepeatAcrossSlices(t *testing.T) {
	m, d := toolRounds(t, 3, true)
	graph, store := controlGraph(t.Name(), m, d, stdlib.ToolLoopOpts{MaxIterations: 2, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 5}}), loom.NewMemStore()
	result, err := graph.Run(context.Background(), controlInput(), store)
	if err != nil {
		t.Fatal(err)
	}
	controlOutcome(t, result, stdlib.ToolLoopSliceLimit, 2)
	delta, err := stdlib.PrepareToolLoopResume(result.State, controlGrant(t, result, "new", 5))
	if err != nil {
		t.Fatal(err)
	}
	result, err = graph.Resume(context.Background(), result.RunID, delta, store)
	if err != nil {
		t.Fatal(err)
	}
	controlOutcome(t, result, stdlib.ToolLoopRepeatLimit, 3)
	if m.calls != 3 || len(d.dispatched) != 2 || len(messagesFromState(t, result.State)) != 5 {
		t.Fatalf("repeated batch was dispatched or entered tool history: %#v", result.State)
	}
}

func TestControlledToolLoopParkConsumesLastRound(t *testing.T) {
	m, d := toolRounds(t, 1, false)
	d.results["call-1"] = contract.ToolResult{Park: true, ParkRef: "approval"}
	graph, store := controlGraph(t.Name(), m, d, stdlib.ToolLoopOpts{MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 2}}), loom.NewMemStore()
	result, err := graph.Run(context.Background(), controlInput(), store)
	if err != nil {
		t.Fatal(err)
	}
	controlOutcome(t, result, stdlib.ToolLoopAwaitToolResult, 1)
	_, err = stdlib.PrepareToolLoopResume(result.State, controlGrant(t, result, "not-approval", 3))
	if err == nil {
		t.Fatal("round grant approved parked tools")
	}
	result, err = graph.Resume(context.Background(), result.RunID, loom.State{"__resumed_tool_results": map[string]any{"call-1": map[string]any{"content": "written", "is_error": false}}}, store)
	if err != nil {
		t.Fatal(err)
	}
	controlOutcome(t, result, stdlib.ToolLoopSliceLimit, 1)
	if m.calls != 1 || len(d.dispatched) != 1 || len(d.executed) != 0 {
		t.Fatal("approval restore added a round or re-dispatched the parked call")
	}
	assertExactlyOneToolResultPerCall(t, messagesFromState(t, result.State))
}

type failingControlStore struct{ *loom.MemStore }

var errControlSave = errors.New("control save unavailable")

func (failingControlStore) Put(context.Context, string, string, []byte) error { return errControlSave }

func TestControlledToolLoopRequiredSave(t *testing.T) {
	m, d := toolRounds(t, 1, false)
	graph := controlGraph(t.Name(), m, d, stdlib.ToolLoopOpts{MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 3}})
	result, err := graph.Run(context.Background(), controlInput(), failingControlStore{loom.NewMemStore()})
	if !errors.Is(err, errControlSave) || result.StopReason != loom.StopError || result.Yielded {
		t.Fatalf("save failure reported a resumable pause: result %#v, err %v", result, err)
	}
}

func TestControlledToolLoopPreservesEntrySnapshot(t *testing.T) {
	m, d := toolRounds(t, 1, false)
	opts := stdlib.ToolLoopOpts{MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 3}}
	step := stdlib.NewToolLoopStep(m, d, opts)
	state := controlInput()
	state["__run_id"] = "run"
	before := decodeStateValue[loom.State](t, state)
	delta, err := step(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, decodeStateValue[loom.State](t, state)) {
		t.Fatal("Step mutated its entry state")
	}
	if _, hasOutput := delta["output"]; hasOutput {
		t.Fatal("slice limit returned final output")
	}
}

func TestControlledToolLoopStagesStateOpsUntilCompletion(t *testing.T) {
	for _, stopTool := range []bool{false, true} {
		t.Run(fmt.Sprint(stopTool), func(t *testing.T) {
			m, d := toolRounds(t, 2, false)
			d.results["call-1"] = contract.ToolResult{Content: "first", StateOps: []contract.StateOp{
				{Type: "debit", Key: "balance", Value: 2}, {Type: "delete", Key: "scratch"},
			}}
			d.results["call-2"] = contract.ToolResult{Content: "second", StopLoop: stopTool,
				StateOps:   []contract.StateOp{{Type: "debit", Key: "balance", Value: 3}},
				StatePatch: map[string]any{"receipt": "saved"}}
			if !stopTool {
				m.scripts = append(m.scripts, func(contract.ChatRequest) contract.ChatResponse { return contract.ChatResponse{Content: "finished"} })
			}
			opts := stdlib.ToolLoopOpts{MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 3},
				StatePatchPolicy: stdlib.AllowKeys("work", "balance", "scratch", "receipt")}
			graph, store := controlGraph(t.Name(), m, d, opts), loom.NewMemStore()
			initial := controlInput()
			initial["balance"], initial["scratch"] = float64(10), "keep until completion"
			result, err := graph.Run(context.Background(), initial, store)
			if err != nil {
				t.Fatal(err)
			}
			for round := 1; result.Yielded; round++ {
				controlOutcome(t, result, stdlib.ToolLoopSliceLimit, uint64(round))
				if result.State["balance"] != float64(10) || result.State["scratch"] != "keep until completion" || result.State["receipt"] != nil {
					t.Fatalf("paused state committed staged changes: %#v", result.State)
				}
				delta, err := stdlib.PrepareToolLoopResume(result.State, controlGrant(t, result, fmt.Sprintf("grant-%d", round), 3))
				if err != nil {
					t.Fatal(err)
				}
				result, err = graph.Resume(context.Background(), result.RunID, delta, store)
				if err != nil {
					t.Fatal(err)
				}
			}
			reason, used := stdlib.ToolLoopFinalResponse, uint64(3)
			if stopTool {
				reason, used = stdlib.ToolLoopToolStop, 2
			}
			controlOutcome(t, result, reason, used)
			if result.State["balance"] != float64(5) || result.State["receipt"] != "saved" {
				t.Fatalf("StateOps were not applied once: %#v", result.State)
			}
			if _, present := result.State["scratch"]; present {
				t.Fatal("StateOps delete did not reach the Graph merge")
			}
			if len(d.executed) != 2 || uint64(m.calls) != used {
				t.Fatalf("effects were repeated: models %d, tools %#v", m.calls, d.executed)
			}
		})
	}
}

func TestControlledToolLoopRejectsInvalidSnapshots(t *testing.T) {
	m, d := toolRounds(t, 1, false)
	opts := stdlib.ToolLoopOpts{MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 3}}
	graph, store := controlGraph(t.Name(), m, d, opts), loom.NewMemStore()
	result, err := graph.Run(context.Background(), controlInput(), store)
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(loom.State, map[string]any){
		"missing counter":          func(_ loom.State, c map[string]any) { delete(c, "slice_rounds_used") },
		"null counter":             func(_ loom.State, c map[string]any) { c["slice_rounds_used"] = nil },
		"fractional counter":       func(_ loom.State, c map[string]any) { c["slice_rounds_used"] = 0.5 },
		"negative counter":         func(_ loom.State, c map[string]any) { c["total_rounds_used"] = -1 },
		"counter overflow":         func(_ loom.State, c map[string]any) { c["authorized_total_rounds"] = uint64(1 << 53) },
		"contradicting total":      func(_ loom.State, c map[string]any) { c["total_rounds_used"] = 2 },
		"wrong run":                func(s loom.State, _ map[string]any) { s["__run_id"] = "fork" },
		"unknown version":          func(_ loom.State, c map[string]any) { c["version"] = 2 },
		"unknown field":            func(_ loom.State, c map[string]any) { c["guess"] = true },
		"missing private messages": func(s loom.State, _ map[string]any) { delete(s, "__toolloop_msgs") },
		"missing pending field":    func(s loom.State, _ map[string]any) { delete(s, "__toolloop_pending") },
		"null pending field":       func(s loom.State, _ map[string]any) { s["__toolloop_pending"] = nil },
		"unmatched tool result": func(s loom.State, _ map[string]any) {
			s["__toolloop_msgs"] = []contract.Message{{Role: "tool", ToolCallID: "invented"}}
		},
		"invented pending call": func(s loom.State, _ map[string]any) {
			s["__toolloop_pending"] = []map[string]any{{"call_id": "invented", "tool": "work", "args": `{}`, "park_ref": "approval"}}
		},
		"halted response at round limit": func(_ loom.State, c map[string]any) {
			c["halted_response"] = contract.ChatResponse{Content: "not safe"}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			state := decodeStateValue[loom.State](t, result.State)
			control := state["__toolloop_control"].(map[string]any)
			mutate(state, control)
			if _, _, err := stdlib.ReadToolLoopOutcome(state); err == nil {
				t.Fatal("corrupt protocol was readable as a valid outcome")
			}
			if _, err := stdlib.NewToolLoopStep(m, d, opts)(context.Background(), state); err == nil {
				t.Fatal("corrupt protocol was executable")
			}
		})
	}
	if m.calls != 1 || len(d.executed) != 1 {
		t.Fatal("corrupt snapshot caused new effects")
	}
}

func TestControlledToolLoopRejectsChangedPolicyAndNestedExecution(t *testing.T) {
	m, d := toolRounds(t, 1, false)
	opts := stdlib.ToolLoopOpts{MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 3}}
	graph, store := controlGraph(t.Name(), m, d, opts), loom.NewMemStore()
	result, err := graph.Run(context.Background(), controlInput(), store)
	if err != nil {
		t.Fatal(err)
	}
	changed := opts
	changed.MaxIterations = 2
	if _, err := stdlib.NewToolLoopStep(m, d, changed)(context.Background(), result.State); err == nil {
		t.Fatal("changed slice policy silently reset the loop")
	}
	changed.Control = nil
	if _, err := stdlib.NewToolLoopStep(m, d, changed)(context.Background(), result.State); err == nil {
		t.Fatal("removing Control downgraded a controlled snapshot")
	}
	parent := loom.NewGraph("parent", "child")
	parent.AddStep("child", stdlib.NewSubGraphStep(controlGraph("child", m, d, opts), store), loom.End())
	if _, err := parent.Run(context.Background(), controlInput(), store); err == nil {
		t.Fatal("unconnected nested control path was enabled")
	}
	if m.calls != 1 || len(d.executed) != 1 {
		t.Fatal("policy mismatch or nested execution reached effects")
	}
}

func TestControlledToolLoopNestedGuardDoesNotPreventEarlierChildEffects(t *testing.T) {
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
		t.Run(constructor.name, func(t *testing.T) {
			preludeModel := &parkScriptLLM{t: t, scripts: []func(contract.ChatRequest) contract.ChatResponse{
				func(contract.ChatRequest) contract.ChatResponse {
					return contract.ChatResponse{Content: "prelude completed", StopReason: "stop"}
				},
			}}
			controlledModel, controlledTools := toolRounds(t, 1, false)
			opts := stdlib.ToolLoopOpts{MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 2}}
			child := loom.NewGraph("child", "prelude", loom.WithCheckpointPolicy(loom.CheckpointRequired))
			child.AddStep("prelude", stdlib.NewToolLoopStep(preludeModel, &parkToolDispatcher{}, stdlib.ToolLoopOpts{}), loom.Always("chat"))
			child.AddStep("chat", stdlib.NewToolLoopStep(controlledModel, controlledTools, opts), loom.End())
			store := loom.NewMemStore()
			parent := loom.NewGraph("parent", "child")
			parent.AddStep("child", constructor.make(child, store), loom.End())

			_, err := parent.Run(context.Background(), controlInput(), store)
			if err == nil || !strings.Contains(err.Error(), "controlled continuation is not supported in child graphs") {
				t.Fatalf("nested controlled loop error = %v", err)
			}
			if preludeModel.calls != 1 {
				t.Fatalf("legacy prelude model calls = %d, want 1", preludeModel.calls)
			}
			if controlledModel.calls != 0 || len(controlledTools.dispatched) != 0 {
				t.Fatalf("controlled step reached effects: models %d, tools %#v", controlledModel.calls, controlledTools.dispatched)
			}
		})
	}
}

func TestControlledToolLoopInvalidProviderProtocolHasNoToolEffects(t *testing.T) {
	for name, response := range map[string]contract.ChatResponse{
		"stop with calls":     {StopReason: "stop", ToolCalls: []contract.ToolCall{{ID: "call", Name: "work"}}},
		"calls without calls": {StopReason: "tool_calls"},
		"duplicate call IDs":  {ToolCalls: []contract.ToolCall{{ID: "call", Name: "work"}, {ID: "call", Name: "work"}}},
	} {
		t.Run(name, func(t *testing.T) {
			m := &parkScriptLLM{t: t, scripts: []func(contract.ChatRequest) contract.ChatResponse{func(contract.ChatRequest) contract.ChatResponse { return response }}}
			d := &parkToolDispatcher{}
			graph := controlGraph(t.Name(), m, d, stdlib.ToolLoopOpts{Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 2}})
			result, err := graph.Run(context.Background(), controlInput(), loom.NewMemStore())
			if err == nil || result.StopReason != loom.StopError || len(d.dispatched) != 0 {
				t.Fatalf("invalid provider response reached effects: result %#v, err %v", result, err)
			}
		})
	}
}

func TestControlledToolLoopResumedEntryRemainsImmutable(t *testing.T) {
	m, d := toolRounds(t, 1, false)
	m.scripts = append(m.scripts, func(request contract.ChatRequest) contract.ChatResponse {
		request.Messages[0].Content = "local provider mutation"
		return contract.ChatResponse{Content: "finished"}
	})
	opts := stdlib.ToolLoopOpts{MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 2}}
	graph, store := controlGraph(t.Name(), m, d, opts), loom.NewMemStore()
	result, err := graph.Run(context.Background(), controlInput(), store)
	if err != nil {
		t.Fatal(err)
	}
	before := decodeStateValue[loom.State](t, result.State)
	delta, err := stdlib.PrepareToolLoopResume(result.State, controlGrant(t, result, "new", 2))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, decodeStateValue[loom.State](t, result.State)) {
		t.Fatal("PrepareToolLoopResume mutated its input")
	}
	entry := result.State.Merge(delta, loom.DefaultMergeConfig())
	entryBefore := decodeStateValue[loom.State](t, entry)
	if _, err := stdlib.NewToolLoopStep(m, d, opts)(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(entryBefore, decodeStateValue[loom.State](t, entry)) {
		t.Fatal("resumed Step mutated its journal entry snapshot")
	}
}
