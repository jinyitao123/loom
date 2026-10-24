package stdlib_test

import (
	"context"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

func recordRequests(m *parkScriptLLM) *[]contract.ChatRequest {
	seen := &[]contract.ChatRequest{}
	for i, script := range m.scripts {
		script := script
		m.scripts[i] = func(request contract.ChatRequest) contract.ChatResponse {
			*seen = append(*seen, request)
			return script(request)
		}
	}
	return seen
}

// Temperature, top_p, thinking and the wait budget reach every model call of a
// tool loop: each round, the wrap-up call after the round budget, and each
// round of a loop under round control.
func TestToolLoopSendsSamplingAndThinkingOnEveryCall(t *testing.T) {
	temperature, topP := 0.2, 0.8
	budget := &contract.WaitBudget{IdleMS: 1000}
	opts := stdlib.ToolLoopOpts{Model: "m", Temperature: &temperature, TopP: &topP,
		Thinking: contract.ThinkingDisabled, WaitBudget: budget}
	check := func(t *testing.T, requests []contract.ChatRequest, calls int) {
		t.Helper()
		if len(requests) != calls {
			t.Fatalf("model calls = %d, want %d", len(requests), calls)
		}
		for i, request := range requests {
			if request.Temperature == nil || *request.Temperature != temperature || request.TopP == nil || *request.TopP != topP ||
				request.Thinking != contract.ThinkingDisabled || request.WaitBudget != budget {
				t.Fatalf("call %d = temperature %v top_p %v thinking %q wait %v", i, request.Temperature, request.TopP, request.Thinking, request.WaitBudget)
			}
		}
	}

	t.Run("rounds and wrap-up", func(t *testing.T) {
		m, d := toolRounds(t, 1, false)
		m.scripts = append(m.scripts, func(contract.ChatRequest) contract.ChatResponse {
			return contract.ChatResponse{Content: "summary", StopReason: "stop"}
		})
		seen := recordRequests(m)
		loopOpts := opts
		loopOpts.MaxIterations = 1
		step := stdlib.NewToolLoopStep(m, d, loopOpts)
		if _, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "go"}}}); err != nil {
			t.Fatal(err)
		}
		check(t, *seen, 2)
		if len((*seen)[1].Tools) != 0 {
			t.Fatal("second call is not the wrap-up call")
		}
	})

	t.Run("round control", func(t *testing.T) {
		m, d := toolRounds(t, 1, false)
		m.scripts = append(m.scripts, func(contract.ChatRequest) contract.ChatResponse {
			return contract.ChatResponse{Content: "finished", StopReason: "stop"}
		})
		seen := recordRequests(m)
		loopOpts := opts
		loopOpts.MaxIterations, loopOpts.Control = 3, &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 3}
		result, err := controlGraph(t.Name(), m, d, loopOpts).Run(context.Background(), controlInput(), loom.NewMemStore())
		if err != nil {
			t.Fatal(err)
		}
		controlOutcome(t, result, stdlib.ToolLoopFinalResponse, 2)
		check(t, *seen, 2)
	})
}
