package stdlib_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
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

type statePatchLLM struct {
	responses []contract.ChatResponse
	errAt     int
	requests  []contract.ChatRequest
}

func (m *statePatchLLM) Chat(_ context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	m.requests = append(m.requests, req)
	call := len(m.requests)
	if m.errAt == call {
		return nil, fmt.Errorf("llm failed")
	}
	return &m.responses[call-1], nil
}

func (m *statePatchLLM) Stream(_ context.Context, _ contract.ChatRequest) (<-chan contract.StreamChunk, error) {
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

func TestToolLoop_StatePatchValidation(t *testing.T) {
	tests := []struct {
		name    string
		result  contract.ToolResult
		policy  *stdlib.StatePatchPolicy
		wantErr string
	}{
		{
			name:    "policy is required",
			result:  contract.ToolResult{StatePatch: map[string]any{"route": "agent-a"}},
			wantErr: "policy",
		},
		{
			name:   "key must be allowed",
			result: contract.ToolResult{StatePatch: map[string]any{"route": "agent-a"}},
			policy: &stdlib.StatePatchPolicy{Allowed: map[string]map[string]func(any) error{
				"patch": {"other": nil},
			}},
			wantErr: "not allowed",
		},
		{
			name:   "validator must accept value",
			result: contract.ToolResult{StatePatch: map[string]any{"route": "agent-a"}},
			policy: &stdlib.StatePatchPolicy{Allowed: map[string]map[string]func(any) error{
				"patch": {"route": func(any) error { return fmt.Errorf("invalid route") }},
			}},
			wantErr: "invalid route",
		},
		{
			name:   "value must be JSON serializable",
			result: contract.ToolResult{StatePatch: map[string]any{"route": func() {}}},
			policy: &stdlib.StatePatchPolicy{Allowed: map[string]map[string]func(any) error{
				"patch": {"route": nil},
			}},
			wantErr: "JSON",
		},
		{
			name:    "error result cannot patch state",
			result:  contract.ToolResult{IsError: true, StatePatch: map[string]any{"route": "agent-a"}},
			policy:  stdlib.AllowKeys("patch", "route"),
			wantErr: "IsError",
		},
		{
			name:    "parked result cannot patch state",
			result:  contract.ToolResult{Park: true, StatePatch: map[string]any{"route": "agent-a"}},
			policy:  stdlib.AllowKeys("patch", "route"),
			wantErr: "Park",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call := contract.ToolCall{ID: "1", Name: "patch", Args: `{}`}
			llm := &mockLLM{responses: []contract.ChatResponse{{ToolCalls: []contract.ToolCall{call}}}}
			tools := &mockTools{tools: []contract.ToolDef{{Name: "patch"}}, handler: func(contract.ToolCall) *contract.ToolResult {
				result := tt.result
				result.CallID = call.ID
				return &result
			}}
			step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
				Model: "test", MaxIterations: 1, StatePatchPolicy: tt.policy,
			})

			result, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "go"}}})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
			if result != nil {
				t.Fatalf("result = %#v, want nil on protocol error", result)
			}
		})
	}
}

func TestToolLoop_StateOpsApplyInOrderAfterPatch(t *testing.T) {
	llm := &mockLLM{responses: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "1", Name: "update"}}},
		{Content: "done"},
	}}
	tools := &mockTools{}
	tools.handler = func(call contract.ToolCall) *contract.ToolResult {
		return &contract.ToolResult{
			CallID: call.ID, StatePatch: map[string]any{"balance": float64(20)},
			StateOps: []contract.StateOp{
				{Type: "debit", Key: "balance", Value: float64(3)},
				{Type: "cas", Key: "balance", Expect: float64(17), Value: float64(9)},
				{Type: "replace", Key: "status", Value: "paid"},
				{Type: "delete", Key: "obsolete"},
			},
		}
	}
	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test", StatePatchPolicy: stdlib.AllowKeys("update", "balance", "status", "obsolete"),
	})

	delta, err := step(context.Background(), loom.State{
		"messages": []contract.Message{{Role: "user", Content: "go"}},
		"balance":  float64(100), "obsolete": "yes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if delta["balance"] != float64(9) || delta["status"] != "paid" {
		t.Fatalf("ops result = %#v", delta)
	}
	deleted, ok := delta["__deleted_keys"].([]string)
	if !ok || !reflect.DeepEqual(deleted, []string{"obsolete"}) {
		t.Fatalf("deleted keys = %#v", delta["__deleted_keys"])
	}
}

func TestToolLoop_StateOpsDebitMissingUsesZero(t *testing.T) {
	delta, err := runSingleStateOps(t, loom.State{}, contract.StateOp{Type: "debit", Key: "balance", Value: float64(2)})
	if err != nil {
		t.Fatal(err)
	}
	if delta["balance"] != float64(-2) {
		t.Fatalf("balance = %#v, want -2", delta["balance"])
	}
}

func TestToolLoop_StateOpsCASFailureFailsStep(t *testing.T) {
	_, err := runSingleStateOps(t, loom.State{"status": "old"}, contract.StateOp{
		Type: "cas", Key: "status", Expect: "other", Value: "new",
	})
	if err == nil || !strings.Contains(err.Error(), "cas") {
		t.Fatalf("error = %v, want cas protocol error", err)
	}
}

func TestToolLoop_StateOpsProtocolValidation(t *testing.T) {
	tests := []struct {
		name   string
		tool   string
		op     contract.StateOp
		policy *stdlib.StatePatchPolicy
	}{
		{name: "unauthorized tool", tool: "other", op: contract.StateOp{Type: "replace", Key: "route", Value: "a"}, policy: stdlib.AllowKeys("update", "route")},
		{name: "reserved key", tool: "update", op: contract.StateOp{Type: "delete", Key: "__deleted_keys"}, policy: stdlib.AllowKeys("update", "__deleted_keys")},
		{name: "child protocol key", tool: "update", op: contract.StateOp{Type: "replace", Key: "__child_run_id", Value: "forged"}, policy: stdlib.AllowKeys("update", "__child_run_id")},
		{name: "protocol nil validator", tool: "update", op: contract.StateOp{Type: "replace", Key: "__route", Value: "a"}, policy: stdlib.AllowKeys("update", "__route")},
		{name: "non json value", tool: "update", op: contract.StateOp{Type: "replace", Key: "route", Value: func() {}}, policy: stdlib.AllowKeys("update", "route")},
		{name: "non number debit", tool: "update", op: contract.StateOp{Type: "debit", Key: "route", Value: "one"}, policy: stdlib.AllowKeys("update", "route")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &mockLLM{responses: []contract.ChatResponse{{ToolCalls: []contract.ToolCall{{ID: "1", Name: tt.tool}}}}}
			tools := &mockTools{handler: func(call contract.ToolCall) *contract.ToolResult {
				return &contract.ToolResult{CallID: call.ID, StateOps: []contract.StateOp{tt.op}}
			}}
			step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{Model: "test", StatePatchPolicy: tt.policy})
			result, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "go"}}})
			if err == nil {
				t.Fatalf("result = %#v, want protocol error", result)
			}
		})
	}
}

func TestToolLoop_StateOpsValidateWithOldValue(t *testing.T) {
	var gotOld, gotNew any
	policy := stdlib.AllowKeys("update", "balance")
	policy.ValidateWithState = map[string]map[string]func(old, new any) error{
		"update": {"balance": func(old, new any) error { gotOld, gotNew = old, new; return nil }},
	}
	_, err := runSingleStateOpsWithPolicy(t, loom.State{"balance": float64(10)}, policy,
		contract.StateOp{Type: "debit", Key: "balance", Value: float64(3)})
	if err != nil {
		t.Fatal(err)
	}
	if gotOld != float64(10) || gotNew != float64(7) {
		t.Fatalf("validator saw old=%#v new=%#v", gotOld, gotNew)
	}
}

func TestToolLoop_StatePatchValidateWithOldValue(t *testing.T) {
	var gotOld, gotNew any
	policy := stdlib.AllowKeys("update", "status")
	policy.ValidateWithState = map[string]map[string]func(old, new any) error{
		"update": {"status": func(old, new any) error { gotOld, gotNew = old, new; return nil }},
	}
	llm := &mockLLM{responses: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "1", Name: "update"}}}, {Content: "done"},
	}}
	tools := &mockTools{handler: func(call contract.ToolCall) *contract.ToolResult {
		return &contract.ToolResult{CallID: call.ID, StatePatch: map[string]any{"status": "new"}}
	}}
	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{Model: "test", StatePatchPolicy: policy})
	_, err := step(context.Background(), loom.State{
		"messages": []contract.Message{{Role: "user", Content: "go"}}, "status": "old",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotOld != "old" || gotNew != "new" {
		t.Fatalf("validator saw old=%#v new=%#v", gotOld, gotNew)
	}
}

func runSingleStateOps(t *testing.T, state loom.State, op contract.StateOp) (loom.State, error) {
	t.Helper()
	return runSingleStateOpsWithPolicy(t, state, stdlib.AllowKeys("update", op.Key), op)
}

func runSingleStateOpsWithPolicy(t *testing.T, state loom.State, policy *stdlib.StatePatchPolicy, op contract.StateOp) (loom.State, error) {
	t.Helper()
	llm := &mockLLM{responses: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "1", Name: "update"}}}, {Content: "done"},
	}}
	tools := &mockTools{}
	tools.handler = func(call contract.ToolCall) *contract.ToolResult {
		return &contract.ToolResult{CallID: call.ID, StateOps: []contract.StateOp{op}}
	}
	state["messages"] = []contract.Message{{Role: "user", Content: "go"}}
	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{Model: "test", StatePatchPolicy: policy})
	return step(context.Background(), state)
}

func TestToolLoop_StatePatchMergeAndMessageIsolation(t *testing.T) {
	calls := []contract.ToolCall{
		{ID: "1", Name: "patch", Args: `{}`},
		{ID: "2", Name: "patch", Args: `{}`},
		{ID: "3", Name: "patch", Args: `{}`},
	}
	patches := map[string]map[string]any{
		"1": {"route": "agent-a"},
		"2": {"route": "agent-a"},
		"3": {"count": float64(2)},
	}
	llm := &statePatchLLM{responses: []contract.ChatResponse{
		{ToolCalls: calls},
		{Content: "done"},
	}}
	tools := &mockTools{tools: []contract.ToolDef{{Name: "patch"}}, handler: func(call contract.ToolCall) *contract.ToolResult {
		return &contract.ToolResult{CallID: call.ID, Content: "visible content", StatePatch: patches[call.ID]}
	}}
	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test", StatePatchPolicy: &stdlib.StatePatchPolicy{Allowed: map[string]map[string]func(any) error{
			"patch": {"route": func(value any) error {
				if value != "agent-a" {
					return fmt.Errorf("unexpected route")
				}
				return nil
			},
				"count": nil},
		}},
	})

	result, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "go"}}})
	assertNoError(t, err)
	if result["route"] != "agent-a" || result["count"] != float64(2) {
		t.Fatalf("state patch missing from delta: %#v", result)
	}
	for _, message := range llm.requests[1].Messages {
		if message.Role == "tool" && message.Content != "visible content" {
			t.Fatalf("tool message leaked control data: %#v", message)
		}
	}
}

func TestToolLoop_StatePatchConflict(t *testing.T) {
	calls := []contract.ToolCall{{ID: "1", Name: "patch"}, {ID: "2", Name: "patch"}}
	llm := &mockLLM{responses: []contract.ChatResponse{{ToolCalls: calls}}}
	tools := &mockTools{tools: []contract.ToolDef{{Name: "patch"}}, handler: func(call contract.ToolCall) *contract.ToolResult {
		return &contract.ToolResult{CallID: call.ID, Content: "ok", StatePatch: map[string]any{"route": call.ID}}
	}}
	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test", StatePatchPolicy: stdlib.AllowKeys("patch", "route"),
	})

	result, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "go"}}})
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("error = %v, want state patch conflict", err)
	}
	if result != nil {
		t.Fatalf("result = %#v, want nil", result)
	}
}

func TestToolLoop_StatePatchValidatedAfterPostHooks(t *testing.T) {
	call := contract.ToolCall{ID: "1", Name: "patch"}
	llm := &mockLLM{responses: []contract.ChatResponse{{ToolCalls: []contract.ToolCall{call}}}}
	tools := &mockTools{tools: []contract.ToolDef{{Name: "patch"}}, handler: func(call contract.ToolCall) *contract.ToolResult {
		return &contract.ToolResult{CallID: call.ID, Content: "ok"}
	}}
	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test",
		ToolHooks: []contract.ToolHook{{Post: func(_ context.Context, _ contract.ToolCall, result *contract.ToolResult) error {
			result.StatePatch = map[string]any{"forbidden": true}
			return nil
		}}},
		StatePatchPolicy: stdlib.AllowKeys("patch", "route"),
	})

	result, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "go"}}})
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("error = %v, want post-hook patch validation error", err)
	}
	if result != nil {
		t.Fatalf("result = %#v, want nil", result)
	}
}

func TestToolLoop_StatePatchDiscardedOnLLMError(t *testing.T) {
	call := contract.ToolCall{ID: "1", Name: "patch"}
	llm := &statePatchLLM{responses: []contract.ChatResponse{{ToolCalls: []contract.ToolCall{call}}}, errAt: 2}
	tools := &mockTools{tools: []contract.ToolDef{{Name: "patch"}}, handler: func(call contract.ToolCall) *contract.ToolResult {
		return &contract.ToolResult{CallID: call.ID, Content: "ok", StatePatch: map[string]any{"route": "agent-a"}}
	}}
	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test", StatePatchPolicy: stdlib.AllowKeys("patch", "route"),
	})

	result, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "go"}}})
	if err == nil {
		t.Fatal("expected LLM error")
	}
	if _, ok := result["route"]; ok {
		t.Fatalf("state patch committed on failed turn: %#v", result)
	}
}

func TestToolLoop_StatePatchAuthorizedByActualToolName(t *testing.T) {
	call := contract.ToolCall{ID: "1", Name: "alias"}
	llm := &statePatchLLM{responses: []contract.ChatResponse{{ToolCalls: []contract.ToolCall{call}}, {Content: "done"}}}
	tools := &mockTools{tools: []contract.ToolDef{{Name: "actual"}}, handler: func(call contract.ToolCall) *contract.ToolResult {
		return &contract.ToolResult{CallID: call.ID, Content: "ok", StatePatch: map[string]any{"route": "agent-a"}}
	}}
	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test",
		ToolHooks: []contract.ToolHook{{Pre: func(_ context.Context, call contract.ToolCall) (contract.ToolCall, error) {
			call.Name = "actual"
			return call, nil
		}}},
		StatePatchPolicy: stdlib.AllowKeys("actual", "route"),
	})

	result, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "go"}}})
	assertNoError(t, err)
	if result["route"] != "agent-a" {
		t.Fatalf("route = %#v, want agent-a", result["route"])
	}
}

func TestToolLoop_StatePatchIgnoresPostHookToolNameRewrite(t *testing.T) {
	call := contract.ToolCall{ID: "1", Name: "actual"}
	llm := &mockLLM{responses: []contract.ChatResponse{{ToolCalls: []contract.ToolCall{call}}}}
	tools := &mockTools{handler: func(call contract.ToolCall) *contract.ToolResult {
		return &contract.ToolResult{CallID: call.ID, Content: "ok"}
	}}
	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test",
		ToolHooks: []contract.ToolHook{{Post: func(_ context.Context, _ contract.ToolCall, result *contract.ToolResult) error {
			result.ToolName = "allowed"
			result.StatePatch = map[string]any{"route": "agent-a"}
			return nil
		}}},
		StatePatchPolicy: stdlib.AllowKeys("allowed", "route"),
	})

	result, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "go"}}})
	if err == nil || !strings.Contains(err.Error(), `tool "actual"`) {
		t.Fatalf("error = %v, want authorization against actual dispatch name", err)
	}
	if result != nil {
		t.Fatalf("result = %#v, want nil", result)
	}
}

func TestToolLoop_StatePatchRejectsWrongOrEmptyToolName(t *testing.T) {
	for _, name := range []string{"other", ""} {
		t.Run(fmt.Sprintf("tool_%q", name), func(t *testing.T) {
			call := contract.ToolCall{ID: "1", Name: name}
			llm := &mockLLM{responses: []contract.ChatResponse{{ToolCalls: []contract.ToolCall{call}}}}
			tools := &mockTools{handler: func(call contract.ToolCall) *contract.ToolResult {
				return &contract.ToolResult{CallID: call.ID, Content: "ok", StatePatch: map[string]any{"route": "agent-a"}}
			}}
			step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
				Model: "test", StatePatchPolicy: stdlib.AllowKeys("allowed", "route"),
			})

			result, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "go"}}})
			if err == nil || !strings.Contains(err.Error(), "tool") {
				t.Fatalf("error = %v, want tool authorization error", err)
			}
			if result != nil {
				t.Fatalf("result = %#v, want nil", result)
			}
		})
	}
}

func TestToolLoop_StatePatchRejectsReservedKeysAndNilProtocolValidator(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "output", key: "output"},
		{name: "usage", key: "usage"},
		{name: "toolloop private", key: "__toolloop_staged_patch"},
		{name: "yield private", key: "__yield_reason"},
		{name: "child protocol", key: "__child_pending"},
		{name: "resumed results", key: "__resumed_tool_results"},
		{name: "protocol nil validator", key: "__delegate_to"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call := contract.ToolCall{ID: "1", Name: "patch"}
			llm := &mockLLM{responses: []contract.ChatResponse{{ToolCalls: []contract.ToolCall{call}}}}
			tools := &mockTools{handler: func(call contract.ToolCall) *contract.ToolResult {
				return &contract.ToolResult{CallID: call.ID, StatePatch: map[string]any{tt.key: "value"}}
			}}
			step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
				Model: "test",
				StatePatchPolicy: &stdlib.StatePatchPolicy{Allowed: map[string]map[string]func(any) error{
					"patch": {tt.key: nil},
				}},
			})

			result, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "go"}}})
			if err == nil || !strings.Contains(err.Error(), tt.key) {
				t.Fatalf("error = %v, want rejection mentioning %q", err, tt.key)
			}
			if result != nil {
				t.Fatalf("result = %#v, want nil", result)
			}
		})
	}
}

func TestToolLoop_PostHookErrorDiscardsInjectedPatch(t *testing.T) {
	call := contract.ToolCall{ID: "1", Name: "patch"}
	llm := &mockLLM{responses: []contract.ChatResponse{{ToolCalls: []contract.ToolCall{call}}}}
	tools := &mockTools{handler: func(call contract.ToolCall) *contract.ToolResult {
		return &contract.ToolResult{CallID: call.ID, Content: "ok"}
	}}
	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test",
		ToolHooks: []contract.ToolHook{{Post: func(_ context.Context, _ contract.ToolCall, result *contract.ToolResult) error {
			result.StatePatch = map[string]any{"route": "agent-a"}
			return fmt.Errorf("post authentication failed")
		}}},
		StatePatchPolicy: stdlib.AllowKeys("patch", "route"),
	})

	result, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "go"}}})
	if err == nil || !strings.Contains(err.Error(), "post authentication failed") {
		t.Fatalf("error = %v, want post hook error", err)
	}
	if result != nil {
		t.Fatalf("result = %#v, want nil", result)
	}
}

func TestToolLoop_StatePatchDiscardedOnForcedCompletion(t *testing.T) {
	tests := []struct {
		name       string
		responses  []contract.ChatResponse
		iterations int
		repeats    int
	}{
		{
			name: "max iterations",
			responses: []contract.ChatResponse{
				{ToolCalls: []contract.ToolCall{{ID: "1", Name: "patch"}}},
				{Content: "forced wrap-up"},
			},
			iterations: 1,
		},
		{
			name: "cycle detection",
			responses: []contract.ChatResponse{
				{ToolCalls: []contract.ToolCall{{ID: "1", Name: "patch"}}},
				{ToolCalls: []contract.ToolCall{{ID: "1", Name: "patch"}}, Content: "forced cycle stop"},
			},
			iterations: 3,
			repeats:    2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &statePatchLLM{responses: tt.responses}
			tools := &mockTools{tools: []contract.ToolDef{{Name: "patch"}}, handler: func(call contract.ToolCall) *contract.ToolResult {
				return &contract.ToolResult{CallID: call.ID, Content: "ok", StatePatch: map[string]any{"route": "agent-a"},
					StateOps: []contract.StateOp{{Type: "replace", Key: "status", Value: "staged"}}}
			}}
			step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
				Model: "test", MaxIterations: tt.iterations, MaxToolRepeats: tt.repeats,
				StatePatchPolicy: stdlib.AllowKeys("patch", "route", "status"),
			})

			result, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "go"}}})
			assertNoError(t, err)
			if _, ok := result["route"]; ok {
				t.Fatalf("forced completion committed patch: %#v", result)
			}
			if _, ok := result["status"]; ok {
				t.Fatalf("forced completion committed ops: %#v", result)
			}
		})
	}
}

func TestToolLoop_StatePatchSurvivesParkAndResume(t *testing.T) {
	patchCall := contract.ToolCall{ID: "patch-1", Name: "patch"}
	parkCall := contract.ToolCall{ID: "park-1", Name: "wait"}
	llm := &statePatchLLM{responses: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{patchCall, parkCall}},
		{Content: "done"},
	}}
	tools := &mockTools{
		tools: []contract.ToolDef{{Name: "patch"}, {Name: "wait"}},
		handler: func(call contract.ToolCall) *contract.ToolResult {
			if call.Name == "wait" {
				return &contract.ToolResult{CallID: call.ID, Content: "parked", Park: true, ParkRef: "approval-1"}
			}
			return &contract.ToolResult{CallID: call.ID, Content: "ok", StatePatch: map[string]any{"route": "agent-a"},
				StateOps: []contract.StateOp{{Type: "debit", Key: "balance", Value: float64(2)}}}
		},
	}
	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test", StatePatchPolicy: stdlib.AllowKeys("patch", "route", "balance"),
	})

	initial := loom.State{
		"messages": []contract.Message{{Role: "user", Content: "go"}}, "balance": float64(10),
	}
	yielded, err := step(context.Background(), initial)
	assertNoError(t, err)
	yielded = initial.Merge(yielded, nil)
	if yielded["__toolloop_staged_patch"] == nil {
		t.Fatalf("yielded state missing staged patch: %#v", yielded)
	}
	checkpoint, err := json.Marshal(yielded)
	assertNoError(t, err)
	var resumed loom.State
	assertNoError(t, json.Unmarshal(checkpoint, &resumed))
	resumed["__resumed_tool_results"] = map[string]any{
		parkCall.ID: map[string]any{"content": "approved", "is_error": false},
	}
	result, err := step(context.Background(), resumed)
	assertNoError(t, err)
	if result["route"] != "agent-a" {
		t.Fatalf("resumed result lost staged patch: %#v", result)
	}
	if result["balance"] != float64(8) {
		t.Fatalf("resumed result lost staged ops: %#v", result)
	}
	if value, ok := result["__toolloop_staged_patch"]; !ok || value != nil {
		t.Fatalf("staged patch checkpoint not cleared: %#v", result)
	}
}

func TestToolLoop_StopLoopCommitsPatchWithoutAnotherLLMCall(t *testing.T) {
	calls := []contract.ToolCall{
		{ID: "stop-1", Name: "transfer"},
		{ID: "sibling-1", Name: "observe"},
	}
	llm := &statePatchLLM{responses: []contract.ChatResponse{
		{
			ToolCalls: []contract.ToolCall{{ID: "warmup-1", Name: "observe"}},
			Usage:     contract.Usage{InputTokens: 5, OutputTokens: 2, CostUSD: 0.25},
		},
		{
			Content:   "handoff in progress",
			ToolCalls: calls,
			Usage:     contract.Usage{InputTokens: 7, OutputTokens: 3, CostUSD: 0.50},
		},
	}}
	dispatched := make(map[string]int)
	tools := &mockTools{tools: []contract.ToolDef{{Name: "transfer"}, {Name: "observe"}}, handler: func(call contract.ToolCall) *contract.ToolResult {
		dispatched[call.ID]++
		if call.Name == "transfer" {
			return &contract.ToolResult{
				CallID: call.ID, Content: "accepted", StopLoop: true,
				StateOps: []contract.StateOp{{Type: "replace", Key: "route", Value: "agent-a"}},
			}
		}
		return &contract.ToolResult{CallID: call.ID, Content: "observed"}
	}}
	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test", MaxIterations: 2, StatePatchPolicy: stdlib.AllowKeys("transfer", "route"),
	})

	result, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "go"}}})
	assertNoError(t, err)
	if result["route"] != "agent-a" {
		t.Fatalf("stop patch not committed: %#v", result)
	}
	if result["output"] != "handoff in progress" {
		t.Fatalf("output = %#v, want last assistant content", result["output"])
	}
	wantUsage := contract.Usage{InputTokens: 12, OutputTokens: 5, CostUSD: 0.75}
	if result["usage"] != wantUsage {
		t.Fatalf("usage = %#v, want %#v", result["usage"], wantUsage)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("LLM calls = %d, want 2", len(llm.requests))
	}
	for _, call := range calls {
		if dispatched[call.ID] != 1 {
			t.Fatalf("dispatch count for %q = %d, want 1", call.ID, dispatched[call.ID])
		}
	}
}

func TestToolLoop_StopLoopProtocolValidation(t *testing.T) {
	tests := []struct {
		name    string
		result  contract.ToolResult
		policy  *stdlib.StatePatchPolicy
		wantErr string
	}{
		{
			name:    "bare stop",
			result:  contract.ToolResult{StopLoop: true},
			policy:  stdlib.AllowKeys("transfer", "route"),
			wantErr: "StopLoop",
		},
		{
			name: "unauthorized tool",
			result: contract.ToolResult{
				StopLoop: true, StatePatch: map[string]any{"route": "agent-a"},
			},
			policy:  stdlib.AllowKeys("other", "route"),
			wantErr: "not allowed",
		},
		{
			name: "error stop",
			result: contract.ToolResult{
				StopLoop: true, IsError: true, StatePatch: map[string]any{"route": "agent-a"},
			},
			policy:  stdlib.AllowKeys("transfer", "route"),
			wantErr: "IsError",
		},
		{
			name: "parked stop",
			result: contract.ToolResult{
				StopLoop: true, Park: true, StatePatch: map[string]any{"route": "agent-a"},
			},
			policy:  stdlib.AllowKeys("transfer", "route"),
			wantErr: "Park",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call := contract.ToolCall{ID: "stop-1", Name: "transfer"}
			llm := &statePatchLLM{responses: []contract.ChatResponse{{ToolCalls: []contract.ToolCall{call}}}}
			tools := &mockTools{tools: []contract.ToolDef{{Name: "transfer"}}, handler: func(contract.ToolCall) *contract.ToolResult {
				result := tt.result
				result.CallID = call.ID
				return &result
			}}
			step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
				Model: "test", MaxIterations: 1, StatePatchPolicy: tt.policy,
			})

			result, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "go"}}})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
			if result != nil {
				t.Fatalf("result = %#v, want nil on protocol error", result)
			}
		})
	}
}

func TestToolLoop_ParkTakesPriorityOverStopUntilResume(t *testing.T) {
	stopCall := contract.ToolCall{ID: "stop-1", Name: "transfer"}
	parkCall := contract.ToolCall{ID: "park-1", Name: "wait"}
	llm := &statePatchLLM{responses: []contract.ChatResponse{{
		Content:   "handoff in progress",
		ToolCalls: []contract.ToolCall{stopCall, parkCall},
		Usage:     contract.Usage{InputTokens: 11, OutputTokens: 4, CostUSD: 0.03},
	}}}
	tools := &mockTools{
		tools: []contract.ToolDef{{Name: "transfer"}, {Name: "wait"}},
		handler: func(call contract.ToolCall) *contract.ToolResult {
			if call.Name == "wait" {
				return &contract.ToolResult{CallID: call.ID, Content: "parked", Park: true, ParkRef: "approval-1"}
			}
			return &contract.ToolResult{
				CallID: call.ID, Content: "accepted", StopLoop: true,
				StatePatch: map[string]any{"route": "agent-a"},
			}
		},
	}
	step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test", StatePatchPolicy: stdlib.AllowKeys("transfer", "route"),
	})

	yielded, err := step(context.Background(), loom.State{"messages": []contract.Message{{Role: "user", Content: "go"}}})
	assertNoError(t, err)
	if yielded["__yield"] != true {
		t.Fatalf("stop bypassed park: %#v", yielded)
	}
	if yielded["__toolloop_staged_patch"] == nil {
		t.Fatalf("yielded state missing staged stop result: %#v", yielded)
	}
	checkpoint, err := json.Marshal(yielded)
	assertNoError(t, err)
	var resumed loom.State
	assertNoError(t, json.Unmarshal(checkpoint, &resumed))
	resumed["__resumed_tool_results"] = map[string]any{
		parkCall.ID: map[string]any{"content": "approved", "is_error": false},
	}

	result, err := step(context.Background(), resumed)
	assertNoError(t, err)
	if result["route"] != "agent-a" || result["output"] != "handoff in progress" {
		t.Fatalf("resumed stop result = %#v", result)
	}
	wantUsage := contract.Usage{InputTokens: 11, OutputTokens: 4, CostUSD: 0.03}
	if result["usage"] != wantUsage {
		t.Fatalf("usage = %#v, want %#v", result["usage"], wantUsage)
	}
	if len(llm.requests) != 1 {
		t.Fatalf("LLM calls after resume = %d, want 1", len(llm.requests))
	}
}

func TestToolLoop_MaxIterations(t *testing.T) {
	// LLM keeps returning tool calls until the budget runs out. Exhaustion must
	// NOT error the turn — the loop injects a wrap-up note and makes one final
	// no-tools call so the model closes out in text (summary + hand-off hint).
	llm := &mockLLM{responses: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "0", Name: "loop", Args: `{"i":0}`}}},
		{ToolCalls: []contract.ToolCall{{ID: "1", Name: "loop", Args: `{"i":1}`}}},
		{ToolCalls: []contract.ToolCall{{ID: "2", Name: "loop", Args: `{"i":2}`}}},
		{Content: "partial findings; the rest should be delegated"},
	}}
	tools := &mockTools{
		tools: []contract.ToolDef{{Name: "loop", Description: "Loop"}},
		handler: func(call contract.ToolCall) *contract.ToolResult {
			return &contract.ToolResult{CallID: call.ID, Content: "ok"}
		},
	}

	var lastReq contract.ChatRequest
	wrapper := &chatCapture{inner: llm, captured: &lastReq}

	step := stdlib.NewToolLoopStep(wrapper, tools, stdlib.ToolLoopOpts{
		Model: "test", MaxIterations: 3,
	})

	state, err := step(context.Background(), loom.State{
		"messages": []contract.Message{{Role: "user", Content: "go"}},
	})
	if err != nil {
		t.Fatalf("budget exhaustion must wrap up, not error: %v", err)
	}
	out, _ := state["output"].(string)
	if out != "partial findings; the rest should be delegated" {
		t.Fatalf("expected wrap-up content, got %q", out)
	}
	// The wrap-up call must offer NO tools and end with the budget note.
	if len(lastReq.Tools) != 0 {
		t.Fatalf("wrap-up call must not offer tools, got %d", len(lastReq.Tools))
	}
	last := lastReq.Messages[len(lastReq.Messages)-1]
	if last.Role != "user" || !strings.Contains(last.Content, "budget") {
		t.Fatalf("expected budget wrap-up note as final user message, got role=%s content=%q", last.Role, last.Content)
	}
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
