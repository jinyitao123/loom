package stdlib_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

type journalContextKey struct{}

type replayJournal struct {
	responses map[stdlib.OperationKind]json.RawMessage
	performed map[stdlib.OperationKind]int
}

func (*replayJournal) Active(ctx context.Context) bool {
	active, _ := ctx.Value(journalContextKey{}).(bool)
	return active
}

func (journal *replayJournal) Execute(_ context.Context, operation stdlib.JournalOperation, perform stdlib.JournalPerform) (json.RawMessage, error) {
	if raw := journal.responses[operation.Kind]; len(raw) > 0 {
		return raw, nil
	}
	journal.performed[operation.Kind]++
	response, err := perform()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(response)
	if err == nil {
		journal.responses[operation.Kind] = raw
	}
	return raw, err
}

type journalLLM struct{ calls int }

func (llm *journalLLM) Chat(context.Context, contract.ChatRequest) (*contract.ChatResponse, error) {
	llm.calls++
	return &contract.ChatResponse{Content: "observed"}, nil
}

func (*journalLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, nil
}

type journalTools struct{ calls int }

func (*journalTools) ListTools(context.Context) ([]contract.ToolDef, error) {
	return []contract.ToolDef{{Name: "read", ReadOnly: true}}, nil
}

func (tools *journalTools) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	tools.calls++
	return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "result"}, nil
}

func TestJournaledExecutionReplaysModelAndToolOperations(t *testing.T) {
	journal := &replayJournal{responses: map[stdlib.OperationKind]json.RawMessage{}, performed: map[stdlib.OperationKind]int{}}
	innerLLM := &journalLLM{}
	innerTools := &journalTools{}
	llm := stdlib.NewJournaledLLM(innerLLM, journal)
	tools := stdlib.NewJournaledToolDispatcher(innerTools, journal, stdlib.JournaledToolOpts{SerializeWhenActive: true})
	ctx := context.WithValue(context.Background(), journalContextKey{}, true)
	request := contract.ChatRequest{Model: "model", Messages: []contract.Message{{Role: "user", Content: "task"}}}
	call := contract.ToolCall{ID: "call", Name: "read", Args: `{}`}
	for index := 0; index < 2; index++ {
		if _, err := llm.Chat(ctx, request); err != nil {
			t.Fatal(err)
		}
		if _, err := tools.Dispatch(ctx, call); err != nil {
			t.Fatal(err)
		}
	}
	if innerLLM.calls != 1 || innerTools.calls != 1 {
		t.Fatalf("replay repeated effects: model=%d tool=%d", innerLLM.calls, innerTools.calls)
	}
	definitions, err := tools.ListTools(ctx)
	if err != nil || definitions[0].ReadOnly {
		t.Fatalf("active sequential journal exposed parallel tool: %#v err=%v", definitions, err)
	}
	if _, err := llm.Stream(ctx, request); !errors.Is(err, stdlib.ErrJournalStreamingUnsupported) {
		t.Fatalf("journaled stream error = %v", err)
	}
}

func TestJournalWrappersAreTransparentOutsideActiveExecution(t *testing.T) {
	journal := &replayJournal{responses: map[stdlib.OperationKind]json.RawMessage{}, performed: map[stdlib.OperationKind]int{}}
	inner := &journalLLM{}
	llm := stdlib.NewJournaledLLM(inner, journal)
	if _, err := llm.Chat(context.Background(), contract.ChatRequest{}); err != nil || inner.calls != 1 {
		t.Fatalf("inactive journal did not pass through: calls=%d err=%v", inner.calls, err)
	}
}

func TestJournaledToolRejectsInvalidDurableResponseAsFatal(t *testing.T) {
	journal := &replayJournal{
		responses: map[stdlib.OperationKind]json.RawMessage{stdlib.OperationTool: json.RawMessage(`{"broken":`)},
		performed: map[stdlib.OperationKind]int{},
	}
	tools := stdlib.NewJournaledToolDispatcher(&journalTools{}, journal, stdlib.JournaledToolOpts{})
	ctx := context.WithValue(context.Background(), journalContextKey{}, true)
	_, err := tools.Dispatch(ctx, contract.ToolCall{ID: "call", Name: "write", Args: `{}`})
	if !errors.Is(err, stdlib.ErrJournalExecution) {
		t.Fatalf("invalid journal response was not fatal: %v", err)
	}
}
