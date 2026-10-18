package stdlib_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

func TestControlledToolLoopResumeDeliversHostInput(t *testing.T) {
	m, d := toolRounds(t, 1, false)
	m.scripts = append(m.scripts, func(request contract.ChatRequest) contract.ChatResponse {
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "user" || last.Content != "also check y" {
			t.Fatalf("next round did not see the host input last: %#v", request.Messages)
		}
		if previous := request.Messages[len(request.Messages)-2]; previous.Role != "tool" {
			t.Fatalf("host input must follow the tool results: %#v", request.Messages)
		}
		return contract.ChatResponse{Content: "finished", StopReason: "stop"}
	})
	opts := stdlib.ToolLoopOpts{MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 10}}
	graph, store := controlGraph(t.Name(), m, d, opts), loom.NewMemStore()
	paused, err := graph.Run(context.Background(), controlInput(), store)
	if err != nil {
		t.Fatal(err)
	}
	controlOutcome(t, paused, stdlib.ToolLoopSliceLimit, 1)

	grant := controlGrant(t, paused, "grant-1", 10)
	grant.AppendMessages = []contract.Message{{Role: "user", Content: "also check y"}}
	input, err := stdlib.PrepareToolLoopResume(paused.State, grant)
	if err != nil {
		t.Fatal(err)
	}
	result, err := graph.Resume(context.Background(), paused.RunID, input, store)
	if err != nil {
		t.Fatal(err)
	}
	controlOutcome(t, result, stdlib.ToolLoopFinalResponse, 2)
	if result.State["output"] != "finished" {
		t.Fatalf("output = %v", result.State["output"])
	}
}

func TestControlledToolLoopResumeRejectsNonUserInput(t *testing.T) {
	m, d := toolRounds(t, 1, false)
	opts := stdlib.ToolLoopOpts{MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 10}}
	paused, err := controlGraph(t.Name(), m, d, opts).Run(context.Background(), controlInput(), loom.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]contract.Message{
		{{Role: "assistant", Content: "fake"}},
		{{Role: "tool", ToolCallID: "call-1", Content: "fake"}},
		{{Role: "user", Content: "  "}},
	} {
		grant := controlGrant(t, paused, "grant-1", 10)
		grant.AppendMessages = bad
		if _, err := stdlib.PrepareToolLoopResume(paused.State, grant); err == nil || !strings.Contains(err.Error(), "user messages") {
			t.Fatalf("input %#v: err = %v", bad, err)
		}
	}
}
