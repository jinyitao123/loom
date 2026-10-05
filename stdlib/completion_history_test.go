package stdlib_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

// historyVerifier rejects with scripted reasons and records the history each
// candidate saw, accepting once the script is exhausted.
type historyVerifier struct {
	reasons []string
	seen    [][]string
}

func (v *historyVerifier) VerifyCompletion(_ context.Context, candidate stdlib.CompletionCandidate) (stdlib.CompletionDecision, error) {
	v.seen = append(v.seen, append([]string(nil), candidate.PriorRejections...))
	if len(v.seen) > len(v.reasons) {
		return stdlib.CompletionDecision{Accepted: true}, nil
	}
	return stdlib.CompletionDecision{Feedback: "correct it", Reason: v.reasons[len(v.seen)-1]}, nil
}

func TestToolLoopGivesVerifiersStructuredRejectionHistory(t *testing.T) {
	llm := &statePatchLLM{responses: []contract.ChatResponse{
		{Content: "first"},
		{ToolCalls: []contract.ToolCall{{ID: "c1", Name: "lookup", Args: `{}`}}},
		{Content: "second"},
		{Content: "third"},
	}}
	verifier := &historyVerifier{reasons: []string{"needs_review", ""}}
	tools := &mockTools{tools: []contract.ToolDef{{Name: "lookup"}}, handler: func(call contract.ToolCall) *contract.ToolResult {
		return &contract.ToolResult{CallID: call.ID, Content: "found"}
	}}
	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{MaxIterations: 6, CompletionVerifier: verifier})
	result, err := step(context.Background(), toolChoiceState())
	assertNoError(t, err)
	if result["output"] != "third" {
		t.Fatalf("output = %v", result["output"])
	}
	want := [][]string{nil, {"needs_review"}, {"needs_review", ""}}
	if len(verifier.seen) != len(want) {
		t.Fatalf("verifications = %#v", verifier.seen)
	}
	for index := range want {
		if len(verifier.seen[index]) != len(want[index]) || (len(want[index]) > 0 && !reflect.DeepEqual(verifier.seen[index], want[index])) {
			t.Fatalf("candidate %d saw %#v, want %#v", index+1, verifier.seen[index], want[index])
		}
	}
	if _, leaked := result["__toolloop_rejections"]; leaked {
		t.Fatal("a loop that never parked wrote private rejection state")
	}
	for _, message := range llm.requests[len(llm.requests)-1].Messages {
		if strings.Contains(message.Content, "needs_review") {
			t.Fatal("a rejection reason reached the model transcript")
		}
	}
}

func TestToolLoopRejectionHistorySurvivesPark(t *testing.T) {
	call := contract.ToolCall{ID: "write-1", Name: "write_file", Args: `{"path":"a.txt"}`}
	llm := &parkScriptLLM{t: t, scripts: []func(contract.ChatRequest) contract.ChatResponse{
		func(contract.ChatRequest) contract.ChatResponse {
			return contract.ChatResponse{Content: "claimed done"}
		},
		func(contract.ChatRequest) contract.ChatResponse {
			return contract.ChatResponse{ToolCalls: []contract.ToolCall{call}}
		},
		func(contract.ChatRequest) contract.ChatResponse { return contract.ChatResponse{Content: "written"} },
	}}
	tools := &parkToolDispatcher{defs: []contract.ToolDef{{Name: "write_file"}}, results: map[string]contract.ToolResult{
		call.ID: {Content: "parked pending approval", Park: true, ParkRef: "approval"},
	}}
	verifier := &historyVerifier{reasons: []string{"action_missing"}}
	store := loom.NewMemStore()
	newGraph := func() *loom.Graph {
		g := loom.NewGraph(t.Name(), "chat", loom.WithMergeConfig(loom.DefaultMergeConfig()), loom.WithCheckpointPolicy(loom.CheckpointRequired))
		g.AddStep("chat", stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{Model: "test", MaxIterations: 5, CompletionVerifier: verifier}), loom.End())
		return g
	}
	first, err := newGraph().Run(context.Background(), loom.State{"messages": []any{contract.Message{Role: "user", Content: "write it"}}}, store)
	if err != nil {
		t.Fatal(err)
	}
	assertYieldedForApproval(t, first)
	if got := decodeStateValue[[]string](t, first.State["__toolloop_rejections"]); !reflect.DeepEqual(got, []string{"action_missing"}) {
		t.Fatalf("park snapshot history = %#v", got)
	}
	resumed, err := newGraph().Resume(context.Background(), first.RunID, loom.State{
		"__resumed_tool_results": map[string]any{call.ID: map[string]any{"content": "done", "is_error": false}},
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State["output"] != "written" || len(verifier.seen) != 2 || !reflect.DeepEqual(verifier.seen[1], []string{"action_missing"}) {
		t.Fatalf("output=%v seen=%#v", resumed.State["output"], verifier.seen)
	}
	if value, present := resumed.State["__toolloop_rejections"]; !present || value != nil {
		t.Fatalf("finished resume kept private history: %#v (present %v)", value, present)
	}
}

func TestToolLoopParkWithoutRejectionKeepsSnapshotKeys(t *testing.T) {
	call := contract.ToolCall{ID: "write-1", Name: "write_file", Args: `{}`}
	llm := &parkScriptLLM{t: t, scripts: []func(contract.ChatRequest) contract.ChatResponse{
		func(contract.ChatRequest) contract.ChatResponse {
			return contract.ChatResponse{ToolCalls: []contract.ToolCall{call}}
		},
	}}
	tools := &parkToolDispatcher{defs: []contract.ToolDef{{Name: "write_file"}}, results: map[string]contract.ToolResult{
		call.ID: {Content: "parked", Park: true, ParkRef: "approval"},
	}}
	_, _, first := runParkGraph(t, t.Name(), llm, tools)
	assertYieldedForApproval(t, first)
	if _, present := first.State["__toolloop_rejections"]; present {
		t.Fatal("park without any rejection wrote a history key")
	}
}

func TestControlledRejectionHistorySurvivesPauseAndIsBounded(t *testing.T) {
	m := &parkScriptLLM{t: t, scripts: []func(contract.ChatRequest) contract.ChatResponse{
		func(contract.ChatRequest) contract.ChatResponse {
			return contract.ChatResponse{Content: "draft", StopReason: "stop"}
		},
		func(contract.ChatRequest) contract.ChatResponse {
			return contract.ChatResponse{Content: "final", StopReason: "stop"}
		},
	}}
	verifier := &historyVerifier{reasons: []string{"input_review"}}
	opts := stdlib.ToolLoopOpts{MaxIterations: 3, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 1}, CompletionVerifier: verifier, CompletionVerifierID: "history-v1"}
	store := loom.NewMemStore()
	paused, err := controlGraph(t.Name(), m, &parkToolDispatcher{}, opts).Run(context.Background(), controlInput(), store)
	if err != nil {
		t.Fatal(err)
	}
	controlOutcome(t, paused, stdlib.ToolLoopTotalLimit, 1)
	delta, err := stdlib.PrepareToolLoopResume(paused.State, controlGrant(t, paused, "grant", 3))
	if err != nil {
		t.Fatal(err)
	}
	completed, err := controlGraph(t.Name(), m, &parkToolDispatcher{}, opts).Resume(context.Background(), paused.RunID, delta, store)
	if err != nil {
		t.Fatal(err)
	}
	controlOutcome(t, completed, stdlib.ToolLoopFinalResponse, 2)
	if len(verifier.seen) != 2 || !reflect.DeepEqual(verifier.seen[1], []string{"input_review"}) {
		t.Fatalf("resumed candidate saw %#v", verifier.seen)
	}

	tampered := loom.State{}
	for key, value := range paused.State {
		tampered[key] = value
	}
	control := decodeStateValue[map[string]any](t, paused.State["__toolloop_control"])
	oversized := make([]string, 65)
	for index := range oversized {
		oversized[index] = fmt.Sprintf("r%d", index)
	}
	control["completion_rejections"] = oversized
	tampered["__toolloop_control"] = control
	if _, _, err := stdlib.ReadToolLoopOutcome(tampered); err == nil {
		t.Fatal("an unbounded rejection history was accepted from a snapshot")
	}
}

func TestControlledSnapshotWithoutRejectionsOmitsHistory(t *testing.T) {
	m, d := toolRounds(t, 1, false)
	opts := stdlib.ToolLoopOpts{MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 1}}
	paused, err := controlGraph(t.Name(), m, d, opts).Run(context.Background(), controlInput(), loom.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	control := decodeStateValue[map[string]any](t, paused.State["__toolloop_control"])
	if _, present := control["completion_rejections"]; present {
		t.Fatal("a snapshot without rejections carried a history field")
	}
}
