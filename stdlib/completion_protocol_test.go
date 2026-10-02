package stdlib_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

const bareCallExample = `<｜｜DSML｜｜ calls>
<｜｜DSML｜｜ invoke name="lookup">
<｜｜DSML｜｜ parameter name="query" string="true">sample</｜｜DSML｜｜ parameter>
</｜｜DSML｜｜ invoke>
</｜｜DSML｜｜ calls>`

const prefacedCallExample = "I will inspect the supplied material and check the requested details.\n\n" + bareCallExample

func TestBareToolProtocolCompletionRecognition(t *testing.T) {
	for name, example := range map[string]string{
		"calls":                   bareCallExample,
		"prefaced trailing block": prefacedCallExample,
		"CRLF paragraphs":         strings.ReplaceAll(prefacedCallExample, "\n", "\r\n"),
		"after closed fence":      "```text\nA quoted example.\n```\n\n" + bareCallExample,
		"whitespace":              "\n  " + bareCallExample + "\n",
		"single marker":           strings.ReplaceAll(bareCallExample, "｜｜DSML｜｜", "｜DSML｜"),
		"invoke":                  `<｜｜DSML｜｜ invoke name="lookup">sample</｜｜DSML｜｜ invoke>`,
		"empty calls":             `<｜｜DSML｜｜ calls></｜｜DSML｜｜ calls>`,
	} {
		t.Run(name, func(t *testing.T) {
			decision, err := stdlib.RejectBareToolProtocolCompletion(nil).VerifyCompletion(context.Background(), stdlib.CompletionCandidate{Content: example})
			if err != nil || decision.Accepted || !strings.Contains(decision.Feedback, "did not execute") {
				t.Fatalf("bare envelope accepted: decision=%#v err=%v", decision, err)
			}
		})
	}
	for name, example := range map[string]string{
		"inline evidence":       "The supplied table totals 42. No additional lookup was needed.",
		"inline explanation":    "This is a protocol example: " + bareCallExample,
		"following explanation": bareCallExample + "\nThe example above is only text.",
		"code fence":            "```text\n" + bareCallExample + "\n```",
		"open code fence":       "```text\n\n" + bareCallExample,
		"open tilde fence":      "~~~~text\n\n" + bareCallExample,
		"short fence closure":   "````text\n```\n\n" + bareCallExample,
		"indented code":         "    " + strings.ReplaceAll(bareCallExample, "\n", "\n    "),
		"tab indented code":     "\t" + strings.ReplaceAll(bareCallExample, "\n", "\n\t"),
		"blockquote":            "> " + strings.ReplaceAll(bareCallExample, "\n", "\n> "),
		"quoted":                `"` + bareCallExample + `"`,
		"ordinary xml":          `<calls><invoke name="lookup">sample</invoke></calls>`,
		"mention":               "The marker DSML can occur in documentation.",
		"unbalanced envelope":   strings.Replace(bareCallExample, "</｜｜DSML｜｜ parameter>", "</｜｜DSML｜｜ invoke>", 1),
		"incomplete envelope":   strings.TrimSuffix(bareCallExample, "</｜｜DSML｜｜ calls>"),
		"nonparagraph example":  "An unquoted example on the next line:\n" + bareCallExample,
	} {
		t.Run(name, func(t *testing.T) {
			decision, err := stdlib.RejectBareToolProtocolCompletion(nil).VerifyCompletion(context.Background(), stdlib.CompletionCandidate{Content: example})
			if err != nil || !decision.Accepted {
				t.Fatalf("legitimate text rejected: decision=%#v err=%v", decision, err)
			}
		})
	}
}

func TestBareToolProtocolCompletionComposesWithHostVerifier(t *testing.T) {
	wantErr := errors.New("verification unavailable")
	calls := 0
	verifier := stdlib.RejectBareToolProtocolCompletion(stdlib.CompletionVerifierFunc(func(_ context.Context, candidate stdlib.CompletionCandidate) (stdlib.CompletionDecision, error) {
		calls++
		if candidate.Content == "error" {
			return stdlib.CompletionDecision{}, wantErr
		}
		return stdlib.CompletionDecision{Accepted: candidate.Content == `{"ok":true}`, Feedback: "return the required JSON"}, nil
	}))
	for _, example := range []string{bareCallExample, "natural text", `{"ok":true}`, "error"} {
		decision, err := verifier.VerifyCompletion(context.Background(), stdlib.CompletionCandidate{Content: example})
		if decision.Accepted != (example == `{"ok":true}`) || errors.Is(err, wantErr) != (example == "error") {
			t.Fatalf("composition changed host result: decision=%#v err=%v", decision, err)
		}
	}
	if calls != 3 {
		t.Fatalf("bare protocol reached host verifier: calls=%d", calls)
	}
}

func TestBareToolProtocolCompletionUsesRealToolRound(t *testing.T) {
	model := &statePatchLLM{responses: []contract.ChatResponse{
		{Content: prefacedCallExample, Usage: contract.Usage{OutputTokens: 1}},
		{ToolCalls: []contract.ToolCall{{ID: "real-call", Name: "lookup", Args: `{}`}}, Usage: contract.Usage{OutputTokens: 2}},
		{Content: "The actual result is 42.", Usage: contract.Usage{OutputTokens: 3}},
	}}
	executed := 0
	tools := &mockTools{tools: []contract.ToolDef{{Name: "lookup"}}, handler: func(call contract.ToolCall) *contract.ToolResult {
		executed++
		if call.ID != "real-call" || call.Args != `{}` {
			t.Fatalf("text was converted to a tool call: %#v", call)
		}
		return &contract.ToolResult{CallID: call.ID, Content: "42"}
	}}
	step := stdlib.NewToolLoopStep(model, tools, stdlib.ToolLoopOpts{MaxIterations: 3, CompletionVerifier: stdlib.RejectBareToolProtocolCompletion(nil)})
	result, err := step(context.Background(), controlInput())
	if err != nil || result["output"] != "The actual result is 42." || executed != 1 || len(model.requests) != 3 {
		t.Fatalf("correction did not use real tool round: output=%#v executed=%d err=%v", result["output"], executed, err)
	}
	feedback := model.requests[1].Messages
	if len(feedback) != 3 || feedback[1].Content != prefacedCallExample || len(feedback[1].ToolCalls) != 0 || !strings.Contains(feedback[2].Content, "did not execute") {
		t.Fatal("feedback changed the original text into a tool call or omitted correction")
	}
	transcript := model.requests[2].Messages
	if len(transcript) != 5 || transcript[4].Role != "tool" || transcript[4].ToolCallID != "real-call" || transcript[4].Content != "42" {
		t.Fatal("final answer did not receive the actual tool result")
	}
	if result["usage"] != (contract.Usage{OutputTokens: 6}) {
		t.Fatalf("correction usage not accumulated: %#v", result["usage"])
	}
}

func TestBareToolProtocolCompletionFailsClosedAtBudget(t *testing.T) {
	model := &statePatchLLM{responses: []contract.ChatResponse{{Content: prefacedCallExample}, {Content: prefacedCallExample}}}
	step := stdlib.NewToolLoopStep(model, &mockTools{}, stdlib.ToolLoopOpts{MaxIterations: 2, CompletionVerifier: stdlib.RejectBareToolProtocolCompletion(nil)})
	result, err := step(context.Background(), controlInput())
	if !errors.Is(err, stdlib.ErrCompletionUnverified) || result["output"] != nil || len(model.requests) != 2 {
		t.Fatalf("budget exhaustion accepted protocol text: result=%#v requests=%d err=%v", result, len(model.requests), err)
	}
}

func TestBareToolProtocolCompletionAllowsInlineAnswerAndIsOptIn(t *testing.T) {
	for _, opts := range []stdlib.ToolLoopOpts{
		{MaxIterations: 1, CompletionVerifier: stdlib.RejectBareToolProtocolCompletion(nil)},
		{MaxIterations: 1},
	} {
		answer := "The inline material establishes a total of 42."
		if opts.CompletionVerifier == nil {
			answer = bareCallExample
		}
		model := &statePatchLLM{responses: []contract.ChatResponse{{Content: answer}}}
		step := stdlib.NewToolLoopStep(model, &mockTools{}, opts)
		result, err := step(context.Background(), controlInput())
		if err != nil || result["output"] != answer || len(model.requests) != 1 {
			t.Fatalf("zero-tool answer or opt-in changed: output=%#v err=%v", result["output"], err)
		}
	}
}

func TestBareToolProtocolCompletionControlledResume(t *testing.T) {
	model := &statePatchLLM{responses: []contract.ChatResponse{{Content: prefacedCallExample, StopReason: "stop"}, {Content: "The inline material is sufficient.", StopReason: "stop"}}}
	opts := stdlib.ToolLoopOpts{MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 1}, CompletionVerifier: stdlib.RejectBareToolProtocolCompletion(nil), CompletionVerifierID: stdlib.BareToolProtocolCompletionPolicyID}
	graph, store := controlGraph(t.Name(), model, &mockTools{}, opts), loom.NewMemStore()
	paused, err := graph.Run(context.Background(), controlInput(), store)
	if err != nil {
		t.Fatal(err)
	}
	controlOutcome(t, paused, stdlib.ToolLoopTotalLimit, 1)
	if paused.State["output"] != nil {
		t.Fatal("rejected candidate was published as output")
	}
	changedPolicy := opts
	changedPolicy.CompletionVerifierID += ".changed"
	if _, err := stdlib.NewToolLoopStep(model, &mockTools{}, changedPolicy)(context.Background(), paused.State); err == nil || len(model.requests) != 1 {
		t.Fatal("changed completion policy resumed model execution")
	}
	grant, err := stdlib.PrepareToolLoopResume(paused.State, controlGrant(t, paused, "continue", 2))
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the graph to exercise the stored feedback, not closure state.
	completed, err := controlGraph(t.Name(), model, &mockTools{}, opts).Resume(context.Background(), paused.RunID, grant, store)
	if err != nil {
		t.Fatal(err)
	}
	controlOutcome(t, completed, stdlib.ToolLoopFinalResponse, 2)
	if completed.State["output"] != "The inline material is sufficient." || len(model.requests) != 2 {
		t.Fatal("controlled correction did not complete")
	}
	messages := model.requests[1].Messages
	if len(messages) != 3 || messages[1].Content != prefacedCallExample || !strings.Contains(messages[2].Content, "did not execute") {
		t.Fatal("controlled resume lost the rejection feedback")
	}
}
