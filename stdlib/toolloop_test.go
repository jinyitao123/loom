package stdlib_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/anthropic/loom"
	"github.com/anthropic/loom/contract"
	"github.com/anthropic/loom/stdlib"
)

// mockLLM simulates LLM responses for testing.
type mockLLM struct {
	responses []contract.ChatResponse
	callIndex int
}

func (m *mockLLM) Chat(_ context.Context, _ contract.ChatRequest) (*contract.ChatResponse, error) {
	if m.callIndex >= len(m.responses) {
		return &contract.ChatResponse{Content: "no more responses"}, nil
	}
	resp := m.responses[m.callIndex]
	m.callIndex++
	return &resp, nil
}

func (m *mockLLM) Stream(_ context.Context, _ contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, fmt.Errorf("not implemented in mock")
}

// mockTools simulates tool dispatch.
type mockTools struct {
	tools   []contract.ToolDef
	handler func(call contract.ToolCall) *contract.ToolResult
}

func (m *mockTools) ListTools(_ context.Context) ([]contract.ToolDef, error) {
	return m.tools, nil
}

func (m *mockTools) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if m.handler != nil {
		return m.handler(call), nil
	}
	return &contract.ToolResult{Content: "mock result"}, nil
}

func TestToolLoop_NoToolCalls_ImmediateReturn(t *testing.T) {
	llm := &mockLLM{responses: []contract.ChatResponse{
		{Content: "Hello, world!"},
	}}
	tools := &mockTools{}

	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test", MaxIterations: 10,
	})

	result, err := step(context.Background(), loom.State{
		"messages": []contract.Message{{Role: "user", Content: "Hi"}},
	})
	assertNoError(t, err)
	if result["output"] != "Hello, world!" {
		t.Errorf("output = %v", result["output"])
	}
}

func TestToolLoop_SingleToolCall(t *testing.T) {
	llm := &mockLLM{responses: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "1", Name: "search", Args: `{"q":"test"}`}}},
		{Content: "The answer is 42."},
	}}
	tools := &mockTools{
		tools: []contract.ToolDef{{Name: "search", Description: "Search", InputSchema: json.RawMessage(`{}`)}},
		handler: func(call contract.ToolCall) *contract.ToolResult {
			return &contract.ToolResult{CallID: call.ID, Content: "result: 42"}
		},
	}

	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test", MaxIterations: 10,
	})

	result, err := step(context.Background(), loom.State{
		"messages": []contract.Message{{Role: "user", Content: "What is the answer?"}},
	})
	assertNoError(t, err)
	if result["output"] != "The answer is 42." {
		t.Errorf("output = %v", result["output"])
	}
}

func TestToolLoop_MaxIterations(t *testing.T) {
	// LLM always returns tool calls — should hit max iterations.
	llm := &mockLLM{responses: make([]contract.ChatResponse, 100)}
	for i := range llm.responses {
		llm.responses[i] = contract.ChatResponse{
			ToolCalls: []contract.ToolCall{{ID: fmt.Sprintf("%d", i), Name: "loop", Args: "{}"}},
		}
	}
	tools := &mockTools{
		tools:   []contract.ToolDef{{Name: "loop", Description: "Loop"}},
		handler: func(call contract.ToolCall) *contract.ToolResult { return &contract.ToolResult{CallID: call.ID, Content: "ok"} },
	}

	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test", MaxIterations: 3,
	})

	_, err := step(context.Background(), loom.State{
		"messages": []contract.Message{{Role: "user", Content: "go"}},
	})
	if err == nil {
		t.Fatal("expected max iterations error")
	}
	assertError(t, err, "max iterations")
}

func TestToolLoop_SystemPrompt(t *testing.T) {
	var captured contract.ChatRequest
	llm := &mockLLM{responses: []contract.ChatResponse{{Content: "ok"}}}
	// Intercept the request to check system prompt injection.
	origChat := llm.Chat
	_ = origChat
	// Use a wrapper mock instead.
	wrapper := &chatCapture{
		inner:    llm,
		captured: &captured,
	}

	tools := &mockTools{}
	step := stdlib.NewToolLoopStep(wrapper, tools, stdlib.ToolLoopOpts{
		Model:        "test",
		SystemPrompt: "You are a helpful assistant.",
	})

	_, err := step(context.Background(), loom.State{
		"messages": []contract.Message{{Role: "user", Content: "Hi"}},
	})
	assertNoError(t, err)
	if len(captured.Messages) < 2 {
		t.Fatalf("expected system + user messages, got %d", len(captured.Messages))
	}
	if captured.Messages[0].Role != "system" {
		t.Errorf("first message role = %s, want system", captured.Messages[0].Role)
	}
	if captured.Messages[0].Content != "You are a helpful assistant." {
		t.Errorf("system content = %s", captured.Messages[0].Content)
	}
}

// chatCapture wraps an LLM to capture the request.
type chatCapture struct {
	inner    contract.LLM
	captured *contract.ChatRequest
}

func (c *chatCapture) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	*c.captured = req
	return c.inner.Chat(ctx, req)
}

func (c *chatCapture) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return c.inner.Stream(ctx, req)
}
