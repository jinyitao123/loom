package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

type reactScriptModel struct {
	requests []contract.ChatRequest
}

func (model *reactScriptModel) Chat(_ context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	model.requests = append(model.requests, request)
	switch len(model.requests) {
	case 1:
		return &contract.ChatResponse{StopReason: "tool_calls", ToolCalls: []contract.ToolCall{{ID: "write-wrong", Name: "write", Args: `{"value":"wrong"}`}}}, nil
	case 2:
		return &contract.ChatResponse{StopReason: "stop", Content: "finished"}, nil
	case 3:
		return &contract.ChatResponse{StopReason: "tool_calls", ToolCalls: []contract.ToolCall{{ID: "write-correct", Name: "write", Args: `{"value":"correct"}`}}}, nil
	case 4:
		return &contract.ChatResponse{StopReason: "stop", Content: "finished after correction"}, nil
	default:
		return nil, fmt.Errorf("unexpected model round %d", len(model.requests))
	}
}

func (*reactScriptModel) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, fmt.Errorf("streaming is not used")
}

type reactStateTool struct {
	value  string
	writes int
}

type reactDurableJournal struct {
	responses        map[string]json.RawMessage
	pending          map[string]bool
	lostModelReceipt bool
	lostToolReceipt  bool
	unknownNextTool  bool
}

func newReactDurableJournal() *reactDurableJournal {
	return &reactDurableJournal{responses: map[string]json.RawMessage{}, pending: map[string]bool{}}
}

func (*reactDurableJournal) Active(context.Context) bool { return true }

func (journal *reactDurableJournal) Execute(_ context.Context, operation stdlib.JournalOperation, perform stdlib.JournalPerform) (json.RawMessage, error) {
	input, err := json.Marshal(operation.Input)
	if err != nil {
		return nil, err
	}
	key := string(operation.Kind) + ":" + string(input)
	if journal.pending[key] {
		return nil, fmt.Errorf("%w: %s", stdlib.ErrJournalOutcomeUnknown, key)
	}
	if response := journal.responses[key]; len(response) > 0 {
		return append(json.RawMessage(nil), response...), nil
	}
	value, err := perform()
	if err != nil {
		return nil, err
	}
	if operation.Kind == stdlib.OperationTool && journal.unknownNextTool {
		journal.unknownNextTool = false
		journal.pending[key] = true
		return nil, fmt.Errorf("%w: tool receipt was lost", stdlib.ErrJournalOutcomeUnknown)
	}
	response, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	journal.responses[key] = response
	if operation.Kind == stdlib.OperationModel && !journal.lostModelReceipt {
		journal.lostModelReceipt = true
		return nil, errors.New("model receipt was lost after commit")
	}
	if operation.Kind == stdlib.OperationTool && !journal.lostToolReceipt {
		journal.lostToolReceipt = true
		return nil, errors.New("tool receipt was lost after commit")
	}
	return append(json.RawMessage(nil), response...), nil
}

func (*reactStateTool) ListTools(context.Context) ([]contract.ToolDef, error) {
	return []contract.ToolDef{{Name: "write"}}, nil
}

func (tool *reactStateTool) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	tool.writes++
	if strings.Contains(call.Args, `"correct"`) {
		tool.value = "correct"
	} else {
		tool.value = "wrong"
	}
	return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "stored " + tool.value}, nil
}

func TestReActCompletionCorrectionSurvivesControlledResume(t *testing.T) {
	model, tool := &reactScriptModel{}, &reactStateTool{}
	journal := newReactDurableJournal()
	verifier := stdlib.CompletionVerifierFunc(func(_ context.Context, candidate stdlib.CompletionCandidate) (stdlib.CompletionDecision, error) {
		if tool.value == "correct" {
			return stdlib.CompletionDecision{Accepted: true}, nil
		}
		return stdlib.CompletionDecision{Feedback: "stored value must be correct; observed " + tool.value}, nil
	})
	journaledModel := stdlib.NewJournaledLLM(model, journal)
	journaledTool := stdlib.NewJournaledToolDispatcher(tool, journal, stdlib.JournaledToolOpts{SerializeWhenActive: true})
	graph := loom.NewGraph("react-public-acceptance", "agent", loom.WithCheckpointPolicy(loom.CheckpointRequired))
	graph.AddStep("agent", stdlib.NewToolLoopStep(journaledModel, journaledTool, stdlib.ToolLoopOpts{
		MaxIterations:      2,
		Control:            &stdlib.ToolLoopControl{ID: "agent", InitialTotalRounds: 4},
		CompletionVerifier: verifier, CompletionVerifierID: "stored-value-v1",
	}), loom.End())
	store := loom.NewMemStore()
	input := func() loom.State {
		return loom.State{"messages": []contract.Message{{Role: "user", Content: "store the correct value"}}}
	}
	if _, err := graph.Run(context.Background(), input(), store); err == nil || len(model.requests) != 1 || tool.writes != 0 {
		t.Fatalf("lost model receipt was not recoverable: model=%d writes=%d err=%v", len(model.requests), tool.writes, err)
	}
	if _, err := graph.Run(context.Background(), input(), store); !errors.Is(err, stdlib.ErrJournalExecution) || len(model.requests) != 1 || tool.writes != 1 {
		t.Fatalf("lost tool receipt repeated earlier work: model=%d writes=%d err=%v", len(model.requests), tool.writes, err)
	}
	paused, err := graph.Run(context.Background(), input(), store)
	if err != nil {
		t.Fatal(err)
	}
	outcome, present, err := stdlib.ReadToolLoopOutcome(paused.State)
	if err != nil || !present || outcome.Reason != stdlib.ToolLoopSliceLimit || tool.value != "wrong" {
		t.Fatalf("first slice did not pause after rejected result: outcome=%#v value=%q err=%v", outcome, tool.value, err)
	}
	checkpointSeq, ok := paused.State["__checkpoint_seq"].(int64)
	if !ok {
		if decoded, isNumber := paused.State["__checkpoint_seq"].(float64); isNumber {
			checkpointSeq = int64(decoded)
		} else {
			t.Fatalf("checkpoint sequence = %#v", paused.State["__checkpoint_seq"])
		}
	}
	grant := stdlib.ToolLoopResumeGrant{
		ID: "continue-after-verification", ExpectedRunID: paused.RunID,
		ExpectedCheckpointSeq: checkpointSeq,
		ExpectedYieldToken:    paused.State["__yield_token"].(string), ExpectedSlice: outcome.Slice,
		AuthorizedTotalRounds: 4,
	}
	delta, err := stdlib.PrepareToolLoopResume(paused.State, grant)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := graph.Resume(context.Background(), paused.RunID, delta, store)
	if err != nil {
		t.Fatal(err)
	}
	if completed.StopReason != loom.StopCompleted || completed.State["output"] != "finished after correction" || tool.value != "correct" || tool.writes != 2 {
		t.Fatalf("corrected run did not complete: result=%#v value=%q writes=%d", completed, tool.value, tool.writes)
	}
	if len(model.requests) != 4 || !strings.Contains(model.requests[2].Messages[len(model.requests[2].Messages)-1].Content, "observed wrong") {
		t.Fatalf("verification observation did not survive resume: %#v", model.requests)
	}
}

func TestReActUnknownToolOutcomeStopsWithoutBlindRetry(t *testing.T) {
	tool := &reactStateTool{}
	journal := newReactDurableJournal()
	journal.lostModelReceipt, journal.lostToolReceipt, journal.unknownNextTool = true, true, true
	wrapped := stdlib.NewJournaledToolDispatcher(tool, journal, stdlib.JournaledToolOpts{SerializeWhenActive: true})
	call := contract.ToolCall{ID: "unknown", Name: "write", Args: `{"value":"correct"}`}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := wrapped.Dispatch(context.Background(), call); !errors.Is(err, stdlib.ErrJournalExecution) || !errors.Is(err, stdlib.ErrJournalOutcomeUnknown) {
			t.Fatalf("attempt %d did not surface unknown outcome: %v", attempt+1, err)
		}
	}
	if tool.writes != 1 {
		t.Fatalf("unknown tool outcome was repeated %d times", tool.writes)
	}
}
