package stdlib_test

import (
	"context"
	"errors"
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
		{Role: "assistant", Content: "old action"},
		{Role: "tool", Content: "old observation"},
		{Role: "assistant", Content: "latest"},
	}
	if !policy.Trigger(messages, 11) {
		t.Fatal("threshold did not trigger")
	}
	got, err := policy.Compactor(context.Background(), messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].Role != messages[0].Role || got[0].Content != messages[0].Content ||
		got[2].Role != messages[3].Role || got[2].Content != messages[3].Content ||
		got[3].Role != messages[4].Role || got[3].Content != messages[4].Content {
		t.Fatalf("compacted transcript lost system or tail: %#v", got)
	}
	if got[1].Content != "[Previous conversation summary]\nfacts and pending work" || llm.request.Model != "summary" {
		t.Fatalf("unexpected summary: %#v", got[1])
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
