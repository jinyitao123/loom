package stdlib_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/loom/contract"
	"github.com/anthropic/loom/stdlib"
)

// --- ToolHook tests ---

func TestToolHook_PreBlocks(t *testing.T) {
	blocked := false
	hooks := []contract.ToolHook{{
		Matcher: func(name string) bool { return name == "dangerous_tool" },
		Pre: func(_ context.Context, call contract.ToolCall) (contract.ToolCall, error) {
			blocked = true
			return call, fmt.Errorf("tool %q blocked by policy", call.Name)
		},
	}}

	calls := []contract.ToolCall{
		{ID: "1", Name: "safe_tool", Args: "{}"},
		{ID: "2", Name: "dangerous_tool", Args: `{"cmd":"rm -rf /"}`},
	}
	defs := []contract.ToolDef{{Name: "safe_tool"}, {Name: "dangerous_tool"}}
	tools := &mockTools{handler: func(c contract.ToolCall) *contract.ToolResult {
		return &contract.ToolResult{CallID: c.ID, Content: "ok", ToolName: c.Name}
	}}

	results := stdlib.DispatchWithHooks(context.Background(), tools, calls, defs, hooks)
	if !blocked {
		t.Error("Pre hook should have been called for dangerous_tool")
	}
	if !results[1].IsError {
		t.Error("dangerous_tool result should be an error")
	}
	if results[0].IsError {
		t.Error("safe_tool result should not be an error")
	}
}

func TestToolHook_PreModifiesArgs(t *testing.T) {
	hooks := []contract.ToolHook{{
		Pre: func(_ context.Context, call contract.ToolCall) (contract.ToolCall, error) {
			call.Args = strings.ReplaceAll(call.Args, "/home/user", "/sandbox/user")
			return call, nil
		},
	}}

	var capturedArgs string
	tools := &mockTools{handler: func(c contract.ToolCall) *contract.ToolResult {
		capturedArgs = c.Args
		return &contract.ToolResult{CallID: c.ID, Content: "ok", ToolName: c.Name}
	}}

	calls := []contract.ToolCall{{ID: "1", Name: "edit", Args: `{"path":"/home/user/file.txt"}`}}
	stdlib.DispatchWithHooks(context.Background(), tools, calls, nil, hooks)

	if strings.Contains(capturedArgs, "/home/user") {
		t.Error("Pre hook should have rewritten path to /sandbox")
	}
	if !strings.Contains(capturedArgs, "/sandbox/user") {
		t.Errorf("args = %s", capturedArgs)
	}
}

func TestToolHook_PostAudits(t *testing.T) {
	var auditLog []string
	hooks := []contract.ToolHook{{
		Post: func(_ context.Context, call contract.ToolCall, result *contract.ToolResult) error {
			auditLog = append(auditLog, fmt.Sprintf("%s: %s", call.Name, result.Content))
			return nil
		},
	}}

	tools := &mockTools{handler: func(c contract.ToolCall) *contract.ToolResult {
		return &contract.ToolResult{CallID: c.ID, Content: "result-" + c.Name, ToolName: c.Name}
	}}

	calls := []contract.ToolCall{{ID: "1", Name: "search"}, {ID: "2", Name: "read"}}
	stdlib.DispatchWithHooks(context.Background(), tools, calls, nil, hooks)

	if len(auditLog) != 2 {
		t.Errorf("audit log len = %d, want 2", len(auditLog))
	}
}

// --- Read/Write dispatch tests ---

func TestReadWriteDispatch_ReadOnlyParallel(t *testing.T) {
	var mu sync.Mutex
	var dispatchOrder []string
	tools := &mockTools{handler: func(c contract.ToolCall) *contract.ToolResult {
		mu.Lock()
		dispatchOrder = append(dispatchOrder, c.Name)
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		return &contract.ToolResult{CallID: c.ID, Content: "ok", ToolName: c.Name}
	}}

	calls := []contract.ToolCall{
		{ID: "1", Name: "read1"}, {ID: "2", Name: "read2"}, {ID: "3", Name: "read3"},
	}
	defs := []contract.ToolDef{
		{Name: "read1", ReadOnly: true},
		{Name: "read2", ReadOnly: true},
		{Name: "read3", ReadOnly: true},
	}

	start := time.Now()
	stdlib.DispatchWithHooks(context.Background(), tools, calls, defs, nil)
	elapsed := time.Since(start)

	if elapsed > 25*time.Millisecond {
		t.Errorf("read-only dispatch took %v, expected parallel (~10ms)", elapsed)
	}
}

func TestReadWriteDispatch_StatefulSerial(t *testing.T) {
	var order []string
	var mu sync.Mutex
	tools := &mockTools{handler: func(c contract.ToolCall) *contract.ToolResult {
		mu.Lock()
		order = append(order, c.Name)
		mu.Unlock()
		return &contract.ToolResult{CallID: c.ID, Content: "ok", ToolName: c.Name}
	}}

	calls := []contract.ToolCall{{ID: "1", Name: "write1"}, {ID: "2", Name: "write2"}}
	defs := []contract.ToolDef{
		{Name: "write1", ReadOnly: false},
		{Name: "write2", ReadOnly: false},
	}

	stdlib.DispatchWithHooks(context.Background(), tools, calls, defs, nil)

	if len(order) != 2 || order[0] != "write1" || order[1] != "write2" {
		t.Errorf("stateful order = %v, want [write1, write2]", order)
	}
}

// --- Compaction tests ---

func TestCompaction_TriggersAtThreshold(t *testing.T) {
	compacted := false
	policy := &stdlib.CompactionPolicy{
		Trigger: func(_ []contract.Message, tokens int) bool { return tokens > 100 },
		Compactor: func(_ context.Context, msgs []contract.Message) ([]contract.Message, error) {
			compacted = true
			return []contract.Message{msgs[0], {Role: "assistant", Content: "[summary]"}}, nil
		},
	}

	msgs := make([]contract.Message, 50)
	for i := range msgs {
		msgs[i] = contract.Message{Role: "user", Content: strings.Repeat("word ", 20)}
	}

	if policy.Trigger(msgs, 500) {
		result, _ := policy.Compactor(context.Background(), msgs)
		if len(result) != 2 {
			t.Errorf("compacted messages = %d, want 2", len(result))
		}
		compacted = true
	}
	if !compacted {
		t.Error("compaction should have triggered")
	}
}

func TestCompaction_PreservesSystemMessage(t *testing.T) {
	policy := &stdlib.CompactionPolicy{
		Trigger: func(_ []contract.Message, tokens int) bool { return tokens > 50 },
		Compactor: func(_ context.Context, msgs []contract.Message) ([]contract.Message, error) {
			return []contract.Message{msgs[0], {Role: "assistant", Content: "[summary]"}}, nil
		},
	}

	msgs := []contract.Message{
		{Role: "system", Content: "You are a test agent."},
		{Role: "user", Content: strings.Repeat("long message ", 100)},
	}

	result, _ := policy.Compactor(context.Background(), msgs)
	if result[0].Role != "system" {
		t.Error("system message not preserved")
	}
	if result[0].Content != "You are a test agent." {
		t.Error("system message content changed")
	}
}
