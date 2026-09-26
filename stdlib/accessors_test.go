package stdlib

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
)

func TestGetMessagesPreservesToolCallsAfterCheckpoint(t *testing.T) {
	want := []contract.Message{
		{Role: "user", Content: "submit this draft"},
		{Role: "assistant", Content: "Checking the draft", ToolCalls: []contract.ToolCall{
			{ID: "call-contract", Name: "read_contract", Args: `{"code":"C-1"}`},
		}},
		{Role: "tool", ToolCallID: "call-contract", Content: `{"status":"draft"}`},
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var restored []any
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	got, err := GetMessages(loom.State{"messages": restored})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checkpoint dropped tool-call history: got %#v, want %#v", got, want)
	}
}

func TestGetMessagesRejectsMalformedCheckpointToolCalls(t *testing.T) {
	_, err := GetMessages(loom.State{"messages": []any{map[string]any{
		"role": "assistant", "tool_calls": []any{map[string]any{"id": 42}},
	}}})
	if err == nil {
		t.Fatal("malformed tool calls must fail before sending an invalid provider request")
	}
}
