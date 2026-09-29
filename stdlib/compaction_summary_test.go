package stdlib_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

type summaryLLM struct {
	err     error
	request contract.ChatRequest
}

func (llm *summaryLLM) Chat(_ context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	llm.request = request
	if llm.err != nil {
		return nil, llm.err
	}
	return &contract.ChatResponse{Content: "facts and pending work"}, nil
}

func (*summaryLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, nil
}

func TestSummaryCompactionPreservesSystemAndTail(t *testing.T) {
	llm := &summaryLLM{}
	policy := stdlib.NewSummaryCompactionPolicy(llm, stdlib.SummaryCompactionOpts{Model: "summary", TokenThreshold: 10, KeepLast: 2})
	messages := []contract.Message{
		{Role: "system", Content: "identity"},
		{Role: "user", Content: "old question"},
		{Role: "assistant", Content: "old action", ToolCalls: []contract.ToolCall{{ID: "old-call", Name: "read", Args: `{}`}}},
		{Role: "tool", Content: "old observation", ToolCallID: "old-call"},
		{Role: "assistant", Content: "latest"},
	}
	if !policy.Trigger(messages, 11) {
		t.Fatal("threshold did not trigger")
	}
	got, err := policy.Compactor(context.Background(), messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[0].Role != messages[0].Role || got[0].Content != messages[0].Content ||
		!reflect.DeepEqual(got[2:], messages[2:]) {
		t.Fatalf("compacted transcript lost system or tail: %#v", got)
	}
	if got[1].Content != "[Previous conversation summary]\nfacts and pending work" || llm.request.Model != "summary" {
		t.Fatalf("unexpected summary: %#v", got[1])
	}
}

func TestSummaryCompactionKeepsCompleteToolCallBatch(t *testing.T) {
	for _, tt := range []struct {
		name     string
		keepLast int
		calls    int
	}{
		{name: "single result with one-message tail", keepLast: 1, calls: 1},
		{name: "two results with one-message tail", keepLast: 1, calls: 2},
		{name: "two results with default tail", keepLast: 2, calls: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := make([]contract.ToolCall, tt.calls)
			messages := []contract.Message{{Role: "system", Content: "identity"}, {Role: "user", Content: "old task"}}
			for i := range calls {
				calls[i] = contract.ToolCall{ID: string(rune('a' + i)), Name: "read", Args: `{}`}
			}
			messages = append(messages, contract.Message{Role: "assistant", ToolCalls: calls})
			for _, call := range calls {
				messages = append(messages, contract.Message{Role: "tool", ToolCallID: call.ID, Content: "result"})
			}
			policy := stdlib.NewSummaryCompactionPolicy(&summaryLLM{}, stdlib.SummaryCompactionOpts{KeepLast: tt.keepLast})
			got, err := policy.Compactor(context.Background(), messages)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(messages) || !reflect.DeepEqual(got[2:], messages[2:]) {
				t.Fatalf("compaction split a tool-call batch: %#v", got)
			}
		})
	}
}

func TestSummaryCompactionFailureKeepsTranscript(t *testing.T) {
	llm := &summaryLLM{err: errors.New("provider unavailable")}
	policy := stdlib.NewSummaryCompactionPolicy(llm, stdlib.SummaryCompactionOpts{KeepLast: 1})
	messages := []contract.Message{{Role: "system"}, {Role: "user", Content: "old"}, {Role: "assistant", Content: "new"}}
	got, err := policy.Compactor(context.Background(), messages)
	if err != nil || len(got) != len(messages) {
		t.Fatalf("failed summary changed transcript: got=%#v err=%v", got, err)
	}
}
