package stdlib_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

type parkScriptLLM struct {
	t       *testing.T
	scripts []func(contract.ChatRequest) contract.ChatResponse
	calls   int
}

func (m *parkScriptLLM) Chat(_ context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	m.t.Helper()
	if m.calls >= len(m.scripts) {
		m.t.Fatalf("unexpected LLM call %d", m.calls+1)
		return nil, nil
	}
	script := m.scripts[m.calls]
	m.calls++
	resp := script(req)
	return &resp, nil
}

func (m *parkScriptLLM) Stream(_ context.Context, _ contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, fmt.Errorf("not implemented in parkScriptLLM")
}

type parkToolDispatcher struct {
	defs       []contract.ToolDef
	results    map[string]contract.ToolResult
	dispatched []string
	executed   []string
}

func (d *parkToolDispatcher) ListTools(_ context.Context) ([]contract.ToolDef, error) {
	return d.defs, nil
}

func (d *parkToolDispatcher) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	result, ok := d.results[call.ID]
	if !ok {
		return nil, fmt.Errorf("unexpected tool call %q", call.ID)
	}
	d.dispatched = append(d.dispatched, call.ID)
	if !result.Park {
		d.executed = append(d.executed, call.ID)
	}
	if result.CallID == "" {
		result.CallID = call.ID
	}
	return &result, nil
}

type pendingView struct {
	CallID  string `json:"call_id"`
	Tool    string `json:"tool"`
	Args    string `json:"args"`
	ParkRef string `json:"park_ref"`
}

func runParkGraph(t *testing.T, name string, llm contract.LLM, tools contract.ToolDispatcher) (*loom.Graph, *loom.MemStore, *loom.RunResult) {
	t.Helper()
	store := loom.NewMemStore()
	g := loom.NewGraph(name, "chat",
		loom.WithMergeConfig(loom.DefaultMergeConfig()),
		loom.WithCheckpointPolicy(loom.CheckpointRequired),
	)
	g.AddStep("chat", stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test", MaxIterations: 10,
	}), loom.End())

	result, err := g.Run(context.Background(), loom.State{
		"messages": []any{contract.Message{Role: "user", Content: "do the work"}},
	}, store)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return g, store, result
}

func decodeStateValue[T any](t *testing.T, raw any) T {
	t.Helper()
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal state value: %v", err)
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("unmarshal state value: %v", err)
	}
	return value
}

func pendingFromState(t *testing.T, state loom.State) []pendingView {
	t.Helper()
	return decodeStateValue[[]pendingView](t, state["__toolloop_pending"])
}

func messagesFromState(t *testing.T, state loom.State) []contract.Message {
	t.Helper()
	return decodeStateValue[[]contract.Message](t, state["__toolloop_msgs"])
}

func assertYieldedForApproval(t *testing.T, result *loom.RunResult) {
	t.Helper()
	if !result.Yielded {
		t.Fatal("expected run to yield")
	}
	if result.StopReason != loom.StopYielded {
		t.Fatalf("StopReason = %q, want %q", result.StopReason, loom.StopYielded)
	}
	if got := result.State["yield_type"]; got != "await_approval" {
		t.Fatalf("yield_type = %v, want await_approval", got)
	}
	if got := result.State["__yield_phase"]; got != "mid_step" {
		t.Fatalf("__yield_phase = %v, want mid_step", got)
	}
}

func assertExactlyOneToolResultPerCall(t *testing.T, messages []contract.Message) {
	t.Helper()
	assistantIndex := -1
	expected := make(map[string]int)
	for i, message := range messages {
		if message.Role != "assistant" || len(message.ToolCalls) == 0 {
			continue
		}
		if assistantIndex != -1 {
			t.Fatal("tool-calling assistant message was replayed")
		}
		assistantIndex = i
		for _, call := range message.ToolCalls {
			expected[call.ID] = 0
		}
	}
	if assistantIndex == -1 {
		t.Fatal("missing assistant tool_calls message")
	}
	for _, message := range messages[assistantIndex+1:] {
		if message.Role != "tool" {
			t.Fatalf("message after assistant tool_calls has role %q, want tool", message.Role)
		}
		if _, ok := expected[message.ToolCallID]; !ok {
			t.Fatalf("unexpected tool result call_id %q", message.ToolCallID)
		}
		expected[message.ToolCallID]++
	}
	for callID, count := range expected {
		if count != 1 {
			t.Fatalf("tool result count for %q = %d, want 1", callID, count)
		}
	}
}

func assertProtocolKeysNil(t *testing.T, state loom.State) {
	t.Helper()
	for _, key := range []string{"__toolloop_pending", "__toolloop_msgs", "__resumed_tool_results"} {
		value, ok := state[key]
		if !ok || value != nil {
			t.Fatalf("terminal state[%q] = %#v (present=%v), want present nil", key, value, ok)
		}
	}
}

func TestParkYields(t *testing.T) {
	call := contract.ToolCall{ID: "write-1", Name: "write_file", Args: `{"path":"a.txt"}`}
	llm := &parkScriptLLM{t: t, scripts: []func(contract.ChatRequest) contract.ChatResponse{
		func(contract.ChatRequest) contract.ChatResponse {
			return contract.ChatResponse{ToolCalls: []contract.ToolCall{call}}
		},
	}}
	tools := &parkToolDispatcher{
		defs: []contract.ToolDef{{Name: "write_file"}},
		results: map[string]contract.ToolResult{
			call.ID: {Content: "parked pending approval", Park: true, ParkRef: "approval-42"},
		},
	}

	_, _, result := runParkGraph(t, "park-yields", llm, tools)

	assertYieldedForApproval(t, result)
	if len(tools.executed) != 0 {
		t.Fatalf("parked tool executed: %v", tools.executed)
	}
	if len(tools.dispatched) != 1 || tools.dispatched[0] != call.ID {
		t.Fatalf("dispatcher calls = %v, want [%s]", tools.dispatched, call.ID)
	}
	messages := messagesFromState(t, result.State)
	for _, message := range messages {
		if strings.Contains(strings.ToLower(message.Content), "parked") {
			t.Fatalf("park placeholder leaked into messages: %#v", message)
		}
	}
	pending := pendingFromState(t, result.State)
	if len(pending) != 1 {
		t.Fatalf("pending = %#v, want one item", pending)
	}
	if got := pending[0]; got.CallID != call.ID || got.Tool != call.Name || got.Args != call.Args || got.ParkRef != "approval-42" {
		t.Fatalf("pending[0] = %#v", got)
	}
}

func TestParkMixedBatch(t *testing.T) {
	readCall := contract.ToolCall{ID: "read-1", Name: "read_file", Args: `{"path":"a.txt"}`}
	writeCall := contract.ToolCall{ID: "write-1", Name: "write_file", Args: `{"path":"a.txt","content":"new"}`}
	llm := &parkScriptLLM{t: t, scripts: []func(contract.ChatRequest) contract.ChatResponse{
		func(contract.ChatRequest) contract.ChatResponse {
			return contract.ChatResponse{ToolCalls: []contract.ToolCall{readCall, writeCall}}
		},
	}}
	tools := &parkToolDispatcher{
		defs: []contract.ToolDef{{Name: "read_file", ReadOnly: true}, {Name: "write_file"}},
		results: map[string]contract.ToolResult{
			readCall.ID:  {Content: "old contents"},
			writeCall.ID: {Content: "parked write", Park: true, ParkRef: "approval-write"},
		},
	}

	_, _, result := runParkGraph(t, "park-mixed", llm, tools)

	assertYieldedForApproval(t, result)
	if len(tools.executed) != 1 || tools.executed[0] != readCall.ID {
		t.Fatalf("executed = %v, want [%s]", tools.executed, readCall.ID)
	}
	messages := messagesFromState(t, result.State)
	toolResults := make(map[string]string)
	for _, message := range messages {
		if message.Role == "tool" {
			toolResults[message.ToolCallID] = message.Content
		}
	}
	if len(toolResults) != 1 || toolResults[readCall.ID] != "old contents" {
		t.Fatalf("tool results in snapshot = %#v", toolResults)
	}
	if _, ok := toolResults[writeCall.ID]; ok {
		t.Fatal("parked write placeholder appeared as a tool result")
	}
	pending := pendingFromState(t, result.State)
	if len(pending) != 1 || pending[0].CallID != writeCall.ID || pending[0].Tool != writeCall.Name || pending[0].Args != writeCall.Args {
		t.Fatalf("pending = %#v", pending)
	}
}

func TestResumeApproved(t *testing.T) {
	call := contract.ToolCall{ID: "write-approve", Name: "write_file", Args: `{"path":"a.txt"}`}
	llm := &parkScriptLLM{t: t, scripts: []func(contract.ChatRequest) contract.ChatResponse{
		func(contract.ChatRequest) contract.ChatResponse {
			return contract.ChatResponse{ToolCalls: []contract.ToolCall{call}}
		},
		func(req contract.ChatRequest) contract.ChatResponse {
			assertExactlyOneToolResultPerCall(t, req.Messages)
			last := req.Messages[len(req.Messages)-1]
			if last.ToolCallID != call.ID || last.Content != "done" {
				t.Fatalf("resumed tool result = %#v", last)
			}
			return contract.ChatResponse{Content: "write completed"}
		},
	}}
	tools := &parkToolDispatcher{
		defs: []contract.ToolDef{{Name: "write_file"}},
		results: map[string]contract.ToolResult{
			call.ID: {Content: "parked pending approval", Park: true, ParkRef: "approval-ok"},
		},
	}
	g, store, first := runParkGraph(t, "resume-approved", llm, tools)
	assertYieldedForApproval(t, first)

	resumed, err := g.Resume(context.Background(), first.RunID, loom.State{
		"__resumed_tool_results": map[string]any{
			call.ID: map[string]any{"content": "done", "is_error": false},
		},
	}, store)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Yielded || resumed.StopReason != loom.StopCompleted {
		t.Fatalf("resume result yielded=%v stop=%q", resumed.Yielded, resumed.StopReason)
	}
	if got := resumed.State["output"]; got != "write completed" {
		t.Fatalf("output = %v", got)
	}
	if llm.calls != 2 {
		t.Fatalf("LLM calls = %d, want 2 (tool_calls request must not replay)", llm.calls)
	}
	if len(tools.dispatched) != 1 {
		t.Fatalf("dispatcher calls = %v, parked call was dispatched again", tools.dispatched)
	}
	assertProtocolKeysNil(t, resumed.State)
}

func TestResumeRejected(t *testing.T) {
	call := contract.ToolCall{ID: "write-reject", Name: "send_email", Args: `{"to":"x@example.com"}`}
	llm := &parkScriptLLM{t: t, scripts: []func(contract.ChatRequest) contract.ChatResponse{
		func(contract.ChatRequest) contract.ChatResponse {
			return contract.ChatResponse{ToolCalls: []contract.ToolCall{call}}
		},
		func(req contract.ChatRequest) contract.ChatResponse {
			assertExactlyOneToolResultPerCall(t, req.Messages)
			last := req.Messages[len(req.Messages)-1]
			if last.ToolCallID != call.ID || last.Content != "approval rejected" {
				t.Fatalf("rejection tool result = %#v", last)
			}
			return contract.ChatResponse{Content: "email was not sent"}
		},
	}}
	tools := &parkToolDispatcher{
		defs: []contract.ToolDef{{Name: "send_email"}},
		results: map[string]contract.ToolResult{
			call.ID: {Content: "parked pending approval", Park: true, ParkRef: "approval-no"},
		},
	}
	g, store, first := runParkGraph(t, "resume-rejected", llm, tools)

	resumed, err := g.Resume(context.Background(), first.RunID, loom.State{
		"__resumed_tool_results": map[string]any{
			call.ID: map[string]any{"content": "approval rejected", "is_error": true},
		},
	}, store)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := resumed.State["output"]; got != "email was not sent" {
		t.Fatalf("output = %v", got)
	}
	assertProtocolKeysNil(t, resumed.State)
}

func TestDoublePark(t *testing.T) {
	firstCall := contract.ToolCall{ID: "write-1", Name: "write_file", Args: `{"path":"a.txt"}`}
	secondCall := contract.ToolCall{ID: "write-2", Name: "send_email", Args: `{"to":"x@example.com"}`}
	llm := &parkScriptLLM{t: t, scripts: []func(contract.ChatRequest) contract.ChatResponse{
		func(contract.ChatRequest) contract.ChatResponse {
			return contract.ChatResponse{ToolCalls: []contract.ToolCall{firstCall, secondCall}}
		},
		func(req contract.ChatRequest) contract.ChatResponse {
			assertExactlyOneToolResultPerCall(t, req.Messages)
			return contract.ChatResponse{Content: "both writes resolved"}
		},
	}}
	tools := &parkToolDispatcher{
		defs: []contract.ToolDef{{Name: "write_file"}, {Name: "send_email"}},
		results: map[string]contract.ToolResult{
			firstCall.ID:  {Content: "parked first", Park: true, ParkRef: "approval-1"},
			secondCall.ID: {Content: "parked second", Park: true, ParkRef: "approval-2"},
		},
	}
	g, store, initial := runParkGraph(t, "double-park", llm, tools)
	assertYieldedForApproval(t, initial)
	if pending := pendingFromState(t, initial.State); len(pending) != 2 {
		t.Fatalf("initial pending = %#v", pending)
	}

	partial, err := g.Resume(context.Background(), initial.RunID, loom.State{
		"__resumed_tool_results": map[string]any{
			firstCall.ID: map[string]any{"content": "first done", "is_error": false},
		},
	}, store)
	if err != nil {
		t.Fatalf("first Resume: %v", err)
	}
	if got := partial.State["__yield_phase"]; got != "mid_step" {
		t.Fatalf("partial __yield_phase = %v, want mid_step", got)
	}
	assertYieldedForApproval(t, partial)
	if llm.calls != 1 {
		t.Fatalf("LLM called before all pending results arrived: %d calls", llm.calls)
	}
	remaining := pendingFromState(t, partial.State)
	if len(remaining) != 1 || remaining[0].CallID != secondCall.ID || remaining[0].ParkRef != "approval-2" {
		t.Fatalf("remaining pending = %#v", remaining)
	}
	partialMessages := messagesFromState(t, partial.State)
	var firstResultCount, secondResultCount int
	for _, message := range partialMessages {
		if message.Role == "tool" && message.ToolCallID == firstCall.ID && message.Content == "first done" {
			firstResultCount++
		}
		if message.Role == "tool" && message.ToolCallID == secondCall.ID {
			secondResultCount++
		}
	}
	if firstResultCount != 1 || secondResultCount != 0 {
		t.Fatalf("partial tool result counts = first:%d second:%d", firstResultCount, secondResultCount)
	}
	if partial.State["__resumed_tool_results"] != nil {
		t.Fatalf("consumed resume results not cleared: %#v", partial.State["__resumed_tool_results"])
	}

	completed, err := g.Resume(context.Background(), partial.RunID, loom.State{
		"__resumed_tool_results": map[string]any{
			secondCall.ID: map[string]any{"content": "second rejected", "is_error": true},
		},
	}, store)
	if err != nil {
		t.Fatalf("second Resume: %v", err)
	}
	if completed.Yielded || completed.State["output"] != "both writes resolved" {
		t.Fatalf("completed yielded=%v output=%v", completed.Yielded, completed.State["output"])
	}
	assertProtocolKeysNil(t, completed.State)
}

func TestNoParkUnchanged(t *testing.T) {
	call := contract.ToolCall{ID: "read-unchanged", Name: "read_file", Args: `{"path":"a.txt"}`}
	llm := &parkScriptLLM{t: t, scripts: []func(contract.ChatRequest) contract.ChatResponse{
		func(contract.ChatRequest) contract.ChatResponse {
			return contract.ChatResponse{ToolCalls: []contract.ToolCall{call}}
		},
		func(req contract.ChatRequest) contract.ChatResponse {
			assertExactlyOneToolResultPerCall(t, req.Messages)
			last := req.Messages[len(req.Messages)-1]
			if last.Content != "ordinary result" {
				t.Fatalf("tool result = %#v", last)
			}
			return contract.ChatResponse{Content: "ordinary completion"}
		},
	}}
	tools := &parkToolDispatcher{
		defs: []contract.ToolDef{{Name: "read_file", ReadOnly: true}},
		results: map[string]contract.ToolResult{
			call.ID: {Content: "ordinary result"},
		},
	}

	_, _, result := runParkGraph(t, "no-park-unchanged", llm, tools)

	if result.Yielded || result.StopReason != loom.StopCompleted {
		t.Fatalf("result yielded=%v stop=%q", result.Yielded, result.StopReason)
	}
	if got := result.State["output"]; got != "ordinary completion" {
		t.Fatalf("output = %v", got)
	}
	if len(tools.executed) != 1 || tools.executed[0] != call.ID {
		t.Fatalf("executed = %v", tools.executed)
	}
	for _, key := range []string{"__toolloop_pending", "__toolloop_msgs", "__resumed_tool_results"} {
		if _, ok := result.State[key]; ok {
			t.Fatalf("no-park path added state key %q", key)
		}
	}
}
