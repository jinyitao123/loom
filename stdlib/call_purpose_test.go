package stdlib_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

// purposeLLM records the purpose marked on each call's context.
type purposeLLM struct {
	inner    contract.LLM
	purposes []contract.CallPurpose
}

func (p *purposeLLM) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	p.purposes = append(p.purposes, contract.CallPurposeOf(ctx))
	return p.inner.Chat(ctx, req)
}

func (p *purposeLLM) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return p.inner.Stream(ctx, req)
}

func TestModelCallsCarryTheirPurpose(t *testing.T) {
	llm := &purposeLLM{inner: &mockLLM{responses: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "1", Name: "read", Args: `{"i":1}`}}},
		{ToolCalls: []contract.ToolCall{{ID: "2", Name: "read", Args: `{"i":2}`}}},
		{Content: "what was found so far"},
	}}}
	tools := &mockTools{tools: []contract.ToolDef{{Name: "read", Description: "Read"}},
		handler: func(call contract.ToolCall) *contract.ToolResult { return &contract.ToolResult{CallID: call.ID, Content: "ok"} }}
	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{Model: "m", MaxIterations: 2})
	if _, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "go"}}}); err != nil {
		t.Fatal(err)
	}
	want := []contract.CallPurpose{contract.CallPurposeGeneration, contract.CallPurposeGeneration, contract.CallPurposeWrapUp}
	if !reflect.DeepEqual(llm.purposes, want) {
		t.Fatalf("tool loop call purposes = %v, want %v", llm.purposes, want)
	}

	summary := &purposeLLM{inner: &mockLLM{responses: []contract.ChatResponse{{Content: "summary"}}}}
	policy := stdlib.NewSummaryCompactionPolicy(summary, stdlib.SummaryCompactionOpts{KeepLast: 1})
	messages := []contract.Message{{Role: "system"}, {Role: "user", Content: "old"}, {Role: "assistant", Content: "older"}, {Role: "assistant", Content: "new"}}
	ctx := contract.WithCallPurpose(context.Background(), contract.CallPurposeGeneration)
	if _, err := policy.Compactor(ctx, messages); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(summary.purposes, []contract.CallPurpose{contract.CallPurposeCompaction}) {
		t.Fatalf("summary call purposes = %v, want compaction even inside a generation", summary.purposes)
	}
}
