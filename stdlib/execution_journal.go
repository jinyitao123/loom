package stdlib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jinyitao123/loom/contract"
)

var ErrJournalStreamingUnsupported = errors.New("journaled execution does not support streaming")

// ErrJournalExecution marks a journal-controlled tool operation that the
// agent loop must stop instead of turning into a model-visible tool error.
var ErrJournalExecution = errors.New("journaled operation failed")

// ErrJournalOutcomeUnknown marks an operation whose effect may have happened
// without a durable response. It must be reconciled, never retried blindly.
var ErrJournalOutcomeUnknown = errors.New("journaled operation outcome is unknown")

type OperationKind string

const (
	OperationModel OperationKind = "model"
	OperationTool  OperationKind = "tool"
)

type JournalOperation struct {
	Kind  OperationKind
	Input any
}

type JournalPerform func() (any, error)

// ExecutionJournal supplies durable operation identity and fencing while Loom
// owns the model/tool decoration and replay contract. Execute implementations
// must return an error wrapping ErrJournalOutcomeUnknown when an effect may have
// happened without a durable response, and must not call perform again for that
// unresolved operation.
//
// A journal that addresses operations by position (the usual design, since a
// model request and a tool call have no identity of their own) needs the loop
// to issue them in the same order on every replay. Wrap tools with
// JournaledToolOpts{SerializeWhenActive: true} so read-only tools are not run
// in parallel while a journal is active; stdlib/recovery_matrix_test.go holds
// the crash-at-every-boundary regression for this contract.
type ExecutionJournal interface {
	Active(context.Context) bool
	Execute(context.Context, JournalOperation, JournalPerform) (json.RawMessage, error)
}

type JournaledToolOpts struct {
	SerializeWhenActive bool
}

func NewJournaledLLM(inner contract.LLM, journal ExecutionJournal) contract.LLM {
	return &journaledLLM{inner: inner, journal: journal}
}

func NewJournaledToolDispatcher(inner contract.ToolDispatcher, journal ExecutionJournal, opts JournaledToolOpts) contract.ToolDispatcher {
	return &journaledTools{inner: inner, journal: journal, opts: opts}
}

type journaledLLM struct {
	inner   contract.LLM
	journal ExecutionJournal
}

func (llm *journaledLLM) Chat(ctx context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	if llm.journal == nil || !llm.journal.Active(ctx) {
		return llm.inner.Chat(ctx, request)
	}
	raw, err := llm.journal.Execute(ctx, JournalOperation{Kind: OperationModel, Input: request}, func() (any, error) {
		return llm.inner.Chat(ctx, request)
	})
	if err != nil {
		return nil, err
	}
	var response contract.ChatResponse
	if len(raw) == 0 || json.Unmarshal(raw, &response) != nil {
		return nil, errors.New("journaled model response is invalid")
	}
	return &response, nil
}

func (llm *journaledLLM) Stream(ctx context.Context, request contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	if llm.journal != nil && llm.journal.Active(ctx) {
		return nil, ErrJournalStreamingUnsupported
	}
	return llm.inner.Stream(ctx, request)
}

type journaledTools struct {
	inner   contract.ToolDispatcher
	journal ExecutionJournal
	opts    JournaledToolOpts
}

func (tools *journaledTools) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	definitions, err := tools.inner.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	if tools.journal != nil && tools.journal.Active(ctx) && tools.opts.SerializeWhenActive {
		definitions = append([]contract.ToolDef(nil), definitions...)
		for index := range definitions {
			definitions[index].ReadOnly = false
		}
	}
	return definitions, nil
}

func (tools *journaledTools) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if tools.journal == nil || !tools.journal.Active(ctx) {
		return tools.inner.Dispatch(ctx, call)
	}
	raw, err := tools.journal.Execute(ctx, JournalOperation{Kind: OperationTool, Input: call}, func() (any, error) {
		return tools.inner.Dispatch(ctx, call)
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJournalExecution, err)
	}
	var result contract.ToolResult
	if len(raw) == 0 || json.Unmarshal(raw, &result) != nil {
		return nil, fmt.Errorf("%w: journaled tool %q response is invalid", ErrJournalExecution, call.Name)
	}
	return &result, nil
}
