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

func TestToolLoopBeforeRoundDeliversInputBetweenRounds(t *testing.T) {
	llm := &statePatchLLM{responses: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "c1", Name: "lookup", Args: "{}"}}},
		{Content: "done"},
	}}
	tools := &mockTools{tools: []contract.ToolDef{{Name: "lookup"}}, handler: func(call contract.ToolCall) *contract.ToolResult {
		return &contract.ToolResult{CallID: call.ID, Content: "found"}
	}}
	var rounds []int
	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "m", MaxIterations: 5,
		BeforeRound: func(_ context.Context, round int, messages []contract.Message) ([]contract.Message, error) {
			rounds = append(rounds, round)
			if round == 1 {
				return append(messages, contract.Message{Role: "user", Content: "also check y"}), nil
			}
			return messages, nil
		},
	})
	result, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "check x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if stdlib.GetString(result, "output", "") != "done" {
		t.Fatalf("output = %v", result["output"])
	}
	if len(rounds) != 2 || rounds[0] != 0 || rounds[1] != 1 {
		t.Fatalf("rounds = %v", rounds)
	}
	second := llm.requests[1].Messages
	if last := second[len(second)-1]; last.Role != "user" || last.Content != "also check y" {
		t.Fatalf("second request did not carry the delivered input: %+v", second)
	}
	if first := llm.requests[0].Messages; len(first) != 1 {
		t.Fatalf("first request changed: %+v", first)
	}
}

func TestToolLoopBeforeRoundCanReplaceHistory(t *testing.T) {
	llm := &statePatchLLM{responses: []contract.ChatResponse{{Content: "ok"}}}
	step := stdlib.NewToolLoopStep(llm, &mockTools{}, stdlib.ToolLoopOpts{
		Model: "m", SystemPrompt: "sys",
		BeforeRound: func(_ context.Context, _ int, messages []contract.Message) ([]contract.Message, error) {
			return []contract.Message{messages[0], {Role: "user", Content: "summary of everything"}}, nil
		},
	})
	history := []contract.Message{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}, {Role: "user", Content: "c"}}
	if _, err := step(context.Background(), loom.State{"messages": history}); err != nil {
		t.Fatal(err)
	}
	sent := llm.requests[0].Messages
	if len(sent) != 2 || sent[0].Role != "system" || sent[1].Content != "summary of everything" {
		t.Fatalf("sent = %+v", sent)
	}
}

func TestToolLoopBeforeRoundErrorStopsTheLoop(t *testing.T) {
	llm := &statePatchLLM{responses: []contract.ChatResponse{{Content: "never"}}}
	boom := errors.New("boom")
	step := stdlib.NewToolLoopStep(llm, &mockTools{}, stdlib.ToolLoopOpts{
		Model: "m",
		BeforeRound: func(context.Context, int, []contract.Message) ([]contract.Message, error) {
			return nil, boom
		},
	})
	_, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "x"}}})
	if !errors.Is(err, boom) || len(llm.requests) != 0 {
		t.Fatalf("err = %v, requests = %d", err, len(llm.requests))
	}
}

func TestToolLoopBeforeRoundRejectedWithControl(t *testing.T) {
	step := stdlib.NewToolLoopStep(&statePatchLLM{}, &mockTools{}, stdlib.ToolLoopOpts{
		Control:     &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 1},
		BeforeRound: func(_ context.Context, _ int, m []contract.Message) ([]contract.Message, error) { return m, nil },
	})
	_, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "BeforeRound") {
		t.Fatalf("err = %v", err)
	}
}
