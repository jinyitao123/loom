package stdlib_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
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

func TestSummaryCompactionPinsTaskAcrossRepeatedReads(t *testing.T) {
	llm := &summaryLLM{}
	policy := stdlib.NewSummaryCompactionPolicy(llm, stdlib.SummaryCompactionOpts{KeepLast: 2, PinTask: true})
	task := contract.Message{Role: "user", Content: "Compare clause 7 of both contracts and list every difference."}
	messages := []contract.Message{{Role: "system", Content: "identity"}, task}
	read := func(id string) {
		messages = append(messages,
			contract.Message{Role: "assistant", ToolCalls: []contract.ToolCall{{ID: id, Name: "read", Args: `{}`}}},
			contract.Message{Role: "tool", ToolCallID: id, Content: "long material " + id})
	}
	for round, id := range []string{"a", "b", "c"} {
		read(id)
		got, err := policy.Compactor(context.Background(), messages)
		if err != nil {
			t.Fatal(err)
		}
		if round > 0 && (len(got) >= len(messages) || got[2].Role != "assistant") {
			t.Fatalf("round %d did not compact: %#v", round, got)
		}
		if !reflect.DeepEqual(got[1], task) {
			t.Fatalf("round %d lost the task: %#v", round, got[1])
		}
		for _, message := range llm.request.Messages {
			if message.Content != "" && strings.Contains(message.Content, task.Content) {
				t.Fatalf("round %d summarized the pinned task", round)
			}
		}
		// The newest read stays whole: its tool call and its result together.
		tail := got[len(got)-2:]
		if tail[0].Role != "assistant" || len(tail[0].ToolCalls) != 1 || tail[1].ToolCallID != tail[0].ToolCalls[0].ID {
			t.Fatalf("round %d split the newest tool exchange: %#v", round, tail)
		}
		messages = got
	}
}

func TestSummaryCompactionDoesNotSummarizeOnlyPinnedTask(t *testing.T) {
	llm := &summaryLLM{}
	policy := stdlib.NewSummaryCompactionPolicy(llm, stdlib.SummaryCompactionOpts{KeepLast: 2, PinTask: true})
	messages := []contract.Message{
		{Role: "system", Content: "identity"},
		{Role: "user", Content: "a very long task"},
		{Role: "assistant", ToolCalls: []contract.ToolCall{{ID: "a", Name: "read", Args: `{}`}}},
		{Role: "tool", ToolCallID: "a", Content: "result"},
	}
	got, err := policy.Compactor(context.Background(), messages)
	if err != nil || !reflect.DeepEqual(got, messages) {
		t.Fatalf("compacted a transcript holding only the pinned task and the newest exchange: %#v %v", got, err)
	}
	if llm.request.Model != "" || len(llm.request.Messages) != 0 {
		t.Fatal("called the summary model with nothing to summarize")
	}
}
