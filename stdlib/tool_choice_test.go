package stdlib_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

// requiredActionVerifier rejects text completions until a tool result exists,
// tagging the rejection with a machine reason for the tool choice policy.
func requiredActionVerifier(reason string) stdlib.CompletionVerifier {
	return stdlib.CompletionVerifierFunc(func(_ context.Context, candidate stdlib.CompletionCandidate) (stdlib.CompletionDecision, error) {
		for _, message := range candidate.Transcript {
			if message.Role == "tool" {
				return stdlib.CompletionDecision{Accepted: true}, nil
			}
		}
		return stdlib.CompletionDecision{Feedback: "the required action has no result", Reason: reason}, nil
	})
}

type recordedChoice struct {
	input  stdlib.ToolChoiceInput
	choice *contract.ToolChoice
}

// forceAfterRejection forces the named tool only right after a matching rejection.
func forceAfterRejection(reason, tool string, seen *[]recordedChoice) stdlib.ToolChoicePolicy {
	return stdlib.ToolChoicePolicyFunc(func(_ context.Context, input stdlib.ToolChoiceInput) (*contract.ToolChoice, error) {
		var choice *contract.ToolChoice
		if input.CompletionRejected && input.CompletionRejectionReason == reason {
			choice = &contract.ToolChoice{Mode: contract.ToolChoiceTool, Name: tool}
		}
		*seen = append(*seen, recordedChoice{input, choice})
		return choice, nil
	})
}

func toolChoiceState() loom.State {
	return loom.State{"messages": []contract.Message{{Role: "user", Content: "convert the record"}}}
}

func TestToolLoopToolChoiceFollowsRejectionOnce(t *testing.T) {
	llm := &statePatchLLM{responses: []contract.ChatResponse{
		{Content: "converted (claimed)"},
		{ToolCalls: []contract.ToolCall{{ID: "c1", Name: "convert", Args: `{"amount":2300}`}}},
		{Content: "converted"},
	}}
	dispatched := 0
	tools := &mockTools{tools: []contract.ToolDef{{Name: "lookup"}, {Name: "convert"}}, handler: func(call contract.ToolCall) *contract.ToolResult {
		dispatched++
		return &contract.ToolResult{CallID: call.ID, Content: "ok"}
	}}
	var seen []recordedChoice
	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		MaxIterations: 5, CompletionVerifier: requiredActionVerifier("required_action_missing"),
		ToolChoicePolicy: forceAfterRejection("required_action_missing", "convert", &seen),
	})
	result, err := step(context.Background(), toolChoiceState())
	assertNoError(t, err)
	if result["output"] != "converted" || dispatched != 1 || len(llm.requests) != 3 {
		t.Fatalf("output=%v dispatched=%d requests=%d", result["output"], dispatched, len(llm.requests))
	}
	want := []*contract.ToolChoice{nil, {Mode: contract.ToolChoiceTool, Name: "convert"}, nil}
	for round, request := range llm.requests {
		got := request.ToolChoice
		if (got == nil) != (want[round] == nil) || (got != nil && *got != *want[round]) {
			t.Fatalf("round %d tool choice = %#v, want %#v", round+1, got, want[round])
		}
	}
	if len(seen) != 3 || seen[0].input.CompletionRejected || !seen[1].input.CompletionRejected ||
		seen[1].input.CompletionRejectionReason != "required_action_missing" || seen[2].input.CompletionRejected || len(seen[1].input.Tools) != 2 {
		t.Fatalf("policy inputs = %#v", seen)
	}
}

func TestToolLoopToolChoiceNotHonoredStopsWithoutDispatch(t *testing.T) {
	for name, forced := range map[string]contract.ChatResponse{
		"text only":  {Content: "converted (claimed again)"},
		"other tool": {ToolCalls: []contract.ToolCall{{ID: "c1", Name: "lookup", Args: `{}`}}},
	} {
		t.Run(name, func(t *testing.T) {
			llm := &statePatchLLM{responses: []contract.ChatResponse{{Content: "converted (claimed)"}, forced}}
			dispatched, verifications := 0, 0
			tools := &mockTools{tools: []contract.ToolDef{{Name: "lookup"}, {Name: "convert"}}, handler: func(call contract.ToolCall) *contract.ToolResult {
				dispatched++
				return &contract.ToolResult{CallID: call.ID, Content: "ok"}
			}}
			var seen []recordedChoice
			verifier := requiredActionVerifier("required_action_missing")
			step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
				MaxIterations: 5,
				CompletionVerifier: stdlib.CompletionVerifierFunc(func(ctx context.Context, candidate stdlib.CompletionCandidate) (stdlib.CompletionDecision, error) {
					verifications++
					return verifier.VerifyCompletion(ctx, candidate)
				}),
				ToolChoicePolicy: forceAfterRejection("required_action_missing", "convert", &seen),
			})
			result, err := step(context.Background(), toolChoiceState())
			if !errors.Is(err, stdlib.ErrToolChoiceNotHonored) || result["__error"] == nil {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			if dispatched != 0 || verifications != 1 || len(llm.requests) != 2 {
				t.Fatalf("dispatched=%d verifications=%d requests=%d", dispatched, verifications, len(llm.requests))
			}
		})
	}
}

func TestToolLoopRejectsInvalidPolicyChoiceBeforeModel(t *testing.T) {
	for name, choice := range map[string]contract.ToolChoice{
		"not offered":   {Mode: contract.ToolChoiceTool, Name: "delete"},
		"unknown mode":  {Mode: "any"},
		"name on mode":  {Mode: contract.ToolChoiceRequired, Name: "convert"},
		"empty tool id": {Mode: contract.ToolChoiceTool},
	} {
		t.Run(name, func(t *testing.T) {
			llm := &statePatchLLM{responses: []contract.ChatResponse{{Content: "unused"}}}
			choice := choice
			step := stdlib.NewToolLoopStep(llm, &mockTools{tools: []contract.ToolDef{{Name: "convert"}}}, stdlib.ToolLoopOpts{
				MaxIterations: 2,
				ToolChoicePolicy: stdlib.ToolChoicePolicyFunc(func(context.Context, stdlib.ToolChoiceInput) (*contract.ToolChoice, error) {
					return &choice, nil
				}),
			})
			if _, err := step(context.Background(), toolChoiceState()); err == nil || len(llm.requests) != 0 {
				t.Fatalf("invalid choice reached the model: err=%v requests=%d", err, len(llm.requests))
			}
		})
	}
	llm := &statePatchLLM{responses: []contract.ChatResponse{{Content: "unused"}}}
	step := stdlib.NewToolLoopStep(llm, &mockTools{}, stdlib.ToolLoopOpts{
		MaxIterations: 2,
		ToolChoicePolicy: stdlib.ToolChoicePolicyFunc(func(context.Context, stdlib.ToolChoiceInput) (*contract.ToolChoice, error) {
			return &contract.ToolChoice{Mode: contract.ToolChoiceRequired}, nil
		}),
	})
	if _, err := step(context.Background(), toolChoiceState()); err == nil || len(llm.requests) != 0 {
		t.Fatalf("choice without offered tools reached the model: err=%v requests=%d", err, len(llm.requests))
	}
}

func TestToolLoopWithoutPolicyKeepsRequestsUnchanged(t *testing.T) {
	llm := &statePatchLLM{responses: []contract.ChatResponse{{Content: "draft"}, {Content: "final"}}}
	step := stdlib.NewToolLoopStep(llm, &mockTools{tools: []contract.ToolDef{{Name: "convert"}}}, stdlib.ToolLoopOpts{
		MaxIterations: 3,
		CompletionVerifier: stdlib.CompletionVerifierFunc(func(_ context.Context, candidate stdlib.CompletionCandidate) (stdlib.CompletionDecision, error) {
			return stdlib.CompletionDecision{Accepted: candidate.Content == "final", Feedback: "again", Reason: "missing"}, nil
		}),
	})
	result, err := step(context.Background(), toolChoiceState())
	assertNoError(t, err)
	if result["output"] != "final" {
		t.Fatalf("output = %v", result["output"])
	}
	for _, request := range llm.requests {
		if request.ToolChoice != nil {
			t.Fatalf("request without a policy carried %#v", request.ToolChoice)
		}
	}
}

func TestControlledToolChoiceRejectionSurvivesRoundLimitPause(t *testing.T) {
	m := &parkScriptLLM{t: t}
	m.scripts = []func(contract.ChatRequest) contract.ChatResponse{
		func(request contract.ChatRequest) contract.ChatResponse {
			if request.ToolChoice != nil {
				t.Fatalf("first round forced %#v", request.ToolChoice)
			}
			return contract.ChatResponse{Content: "converted (claimed)", StopReason: "stop"}
		},
		func(request contract.ChatRequest) contract.ChatResponse {
			if request.ToolChoice == nil || *request.ToolChoice != (contract.ToolChoice{Mode: contract.ToolChoiceTool, Name: "work"}) {
				t.Fatalf("resumed round lost the forced choice: %#v", request.ToolChoice)
			}
			return contract.ChatResponse{ToolCalls: []contract.ToolCall{{ID: "call-1", Name: "work", Args: `{}`}}, StopReason: "tool_calls"}
		},
		func(request contract.ChatRequest) contract.ChatResponse {
			if request.ToolChoice != nil {
				t.Fatalf("choice outlived its round: %#v", request.ToolChoice)
			}
			return contract.ChatResponse{Content: "converted", StopReason: "stop"}
		},
	}
	d := &parkToolDispatcher{defs: []contract.ToolDef{{Name: "work"}}, results: map[string]contract.ToolResult{"call-1": {Content: "done"}}}
	var seen []recordedChoice
	opts := stdlib.ToolLoopOpts{
		MaxIterations: 3, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 1},
		CompletionVerifier: requiredActionVerifier("required_action_missing"), CompletionVerifierID: "fixture-v1",
		ToolChoicePolicy: forceAfterRejection("required_action_missing", "work", &seen), ToolChoicePolicyID: "force-after-rejection-v1",
	}
	graph, store := controlGraph(t.Name(), m, d, opts), loom.NewMemStore()
	paused, err := graph.Run(context.Background(), controlInput(), store)
	if err != nil {
		t.Fatal(err)
	}
	controlOutcome(t, paused, stdlib.ToolLoopTotalLimit, 1)
	delta, err := stdlib.PrepareToolLoopResume(paused.State, controlGrant(t, paused, "grant", 3))
	if err != nil {
		t.Fatal(err)
	}
	// A fresh closure proves the rejection came from the checkpoint, not memory.
	graph = controlGraph(t.Name(), m, d, opts)
	completed, err := graph.Resume(context.Background(), paused.RunID, delta, store)
	if err != nil {
		t.Fatal(err)
	}
	controlOutcome(t, completed, stdlib.ToolLoopFinalResponse, 3)
	if completed.State["output"] != "converted" || len(d.executed) != 1 {
		t.Fatalf("output=%v executed=%v", completed.State["output"], d.executed)
	}
}

func TestControlledToolChoicePolicyRequiresStableID(t *testing.T) {
	policy := stdlib.ToolChoicePolicyFunc(func(context.Context, stdlib.ToolChoiceInput) (*contract.ToolChoice, error) { return nil, nil })
	for name, opts := range map[string]stdlib.ToolLoopOpts{
		"missing id":     {MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 1}, ToolChoicePolicy: policy},
		"missing policy": {MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 1}, ToolChoicePolicyID: "v1"},
	} {
		t.Run(name, func(t *testing.T) {
			model := &parkScriptLLM{t: t}
			result, err := controlGraph(t.Name(), model, &parkToolDispatcher{}, opts).Run(context.Background(), controlInput(), loom.NewMemStore())
			if err == nil || result.StopReason != loom.StopError || model.calls != 0 {
				t.Fatalf("invalid tool choice policy reached model: result=%#v calls=%d err=%v", result, model.calls, err)
			}
		})
	}
}

func TestControlledToolChoicePolicyChangeIsRejectedOnResume(t *testing.T) {
	m := &parkScriptLLM{t: t, scripts: []func(contract.ChatRequest) contract.ChatResponse{
		func(contract.ChatRequest) contract.ChatResponse {
			return contract.ChatResponse{Content: "draft", StopReason: "stop"}
		},
	}}
	var seen []recordedChoice
	opts := stdlib.ToolLoopOpts{
		MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 1},
		CompletionVerifier: requiredActionVerifier("missing"), CompletionVerifierID: "fixture-v1",
		ToolChoicePolicy: forceAfterRejection("missing", "work", &seen), ToolChoicePolicyID: "v1",
	}
	d := &parkToolDispatcher{defs: []contract.ToolDef{{Name: "work"}}}
	graph, store := controlGraph(t.Name(), m, d, opts), loom.NewMemStore()
	paused, err := graph.Run(context.Background(), controlInput(), store)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := stdlib.PrepareToolLoopResume(paused.State, controlGrant(t, paused, "grant", 2))
	if err != nil {
		t.Fatal(err)
	}
	changed := opts
	changed.ToolChoicePolicyID = "v2"
	if _, err := controlGraph(t.Name(), m, d, changed).Resume(context.Background(), paused.RunID, delta, store); err == nil {
		t.Fatal("resume accepted a changed tool choice policy")
	}
}

func TestBareToolProtocolRejectionCarriesReason(t *testing.T) {
	verifier := stdlib.RejectBareToolProtocolCompletion(nil)
	decision, err := verifier.VerifyCompletion(context.Background(), stdlib.CompletionCandidate{
		Content: "<｜DSML｜ calls>\n<｜DSML｜ invoke name=\"work\">\n</｜DSML｜ invoke>\n</｜DSML｜ calls>",
	})
	if err != nil || decision.Accepted || decision.Reason != stdlib.BareToolProtocolRejectionReason {
		t.Fatalf("decision = %#v, err %v", decision, err)
	}
}
