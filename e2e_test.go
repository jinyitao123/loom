package loom_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

// e2eMockLLM simulates LLM responses for E2E testing.
type e2eMockLLM struct {
	responses []contract.ChatResponse
	callIndex int
}

func (m *e2eMockLLM) Chat(_ context.Context, _ contract.ChatRequest) (*contract.ChatResponse, error) {
	if m.callIndex >= len(m.responses) {
		return &contract.ChatResponse{Content: "no more responses"}, nil
	}
	resp := m.responses[m.callIndex]
	m.callIndex++
	return &resp, nil
}

func (m *e2eMockLLM) Stream(_ context.Context, _ contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, fmt.Errorf("not implemented in mock")
}

// e2eMockTools simulates tool dispatch.
type e2eMockTools struct {
	tools   []contract.ToolDef
	handler func(call contract.ToolCall) *contract.ToolResult
}

func (m *e2eMockTools) ListTools(_ context.Context) ([]contract.ToolDef, error) {
	return m.tools, nil
}

func (m *e2eMockTools) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if m.handler != nil {
		return m.handler(call), nil
	}
	return &contract.ToolResult{Content: "mock result"}, nil
}

func TestE2E_SingleAgent_ChatWithTools(t *testing.T) {
	llm := &e2eMockLLM{responses: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "1", Name: "search", Args: `{"q":"test"}`}}},
		{Content: "Based on search results, the answer is 42."},
	}}
	tools := &e2eMockTools{
		tools: []contract.ToolDef{{Name: "search", Description: "Search"}},
		handler: func(call contract.ToolCall) *contract.ToolResult {
			return &contract.ToolResult{CallID: call.ID, Content: "result: 42"}
		},
	}

	g := loom.NewGraph("test-agent", "chat", loom.WithMergeConfig(loom.DefaultMergeConfig()))
	g.AddStep("chat", stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test-model", MaxIterations: 10,
	}), loom.End())

	result, err := g.Run(context.Background(), loom.State{
		"messages": []any{contract.Message{Role: "user", Content: "What is the answer?"}},
	}, nil)
	assertNoError(t, err)
	assertState(t, result, "output", "Based on search results, the answer is 42.")
}

func TestE2E_MultiAgent_Workflow_WithHITL(t *testing.T) {
	store := loom.NewMemStore()
	cfg := loom.DefaultMergeConfig()

	agentA := loom.NewGraph("agent-a", "work", loom.WithMergeConfig(cfg))
	agentA.AddStep("work", echoStep("agent_a_output", "draft document"), loom.End())

	agentB := loom.NewGraph("agent-b", "work", loom.WithMergeConfig(cfg))
	agentB.AddStep("work", echoStep("agent_b_output", "final document"), loom.End())

	wf := loom.NewGraph("workflow", "run_a", loom.WithMergeConfig(cfg))
	wf.AddStep("run_a",
		stdlib.NewSubGraphStep(agentA, store),
		loom.Always("review"))
	wf.AddStep("review",
		stdlib.NewHumanWaitStep("review_prompt", "human_decision"),
		loom.Branch("human_decision", map[string]string{
			"approve": "run_b",
			"reject":  "run_a",
		}, "run_b"))
	wf.AddStep("run_b",
		stdlib.NewSubGraphStep(agentB, store),
		loom.End())

	r1, err := wf.Run(context.Background(), loom.State{}, store)
	assertNoError(t, err)
	assertYielded(t, r1, true)
	assertState(t, r1, "agent_a_output", "draft document")

	r2, err := wf.Resume(context.Background(), r1.RunID, loom.State{"human_decision": "approve"}, store)
	assertNoError(t, err)
	assertYielded(t, r2, false)
	assertState(t, r2, "agent_b_output", "final document")
}

