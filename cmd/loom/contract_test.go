package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// canonicalContract is the loom → weave-daemon wire contract: the exact
// stdout-json (NDJSON) the daemon's LoomBackend.parseLine consumes.
//
// ⚠️ MUST stay in lockstep with the weave consumer-side contract test:
//
//	weave: src/cli/daemon/agent/__tests__/loom.contract.test.ts
//
// loom (producer) asserts it EMITS these exact lines; weave (consumer) asserts
// parseLine maps each into the right AgentMessage / ParsedEvent. If either side
// changes the protocol, its contract test breaks — forcing both to update.
var canonicalContract = []struct {
	line string
	emit func(*Emitter)
}{
	{`{"type":"session_init","session_id":"s1"}`, func(e *Emitter) { _ = e.SessionInit("s1") }},
	{`{"type":"text","text":"hello"}`, func(e *Emitter) { _ = e.Text("hello") }},
	{`{"type":"thinking","text":"hmm"}`, func(e *Emitter) { _ = e.Thinking("hmm") }},
	{`{"type":"tool_use","id":"c1","name":"send_email","input":{"to":"a@b.c"}}`, func(e *Emitter) {
		_ = e.ToolUse("c1", "send_email", json.RawMessage(`{"to":"a@b.c"}`))
	}},
	{`{"type":"tool_result","id":"c1","output":"sent","status":"success"}`, func(e *Emitter) {
		_ = e.ToolResult("c1", "sent", "success")
	}},
	{`{"type":"turn_end","session_id":"s1"}`, func(e *Emitter) { _ = e.TurnEnd("s1") }},
	{`{"type":"error","message":"boom"}`, func(e *Emitter) { _ = e.Error("boom") }},
	{`{"type":"result","status":"completed","output":"done","session_id":"s1"}`, func(e *Emitter) {
		_ = e.Result("completed", "done", "s1")
	}},
}

func jsonEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var ma, mb map[string]any
	if err := json.Unmarshal([]byte(a), &ma); err != nil {
		t.Fatalf("not JSON: %q", a)
	}
	if err := json.Unmarshal([]byte(b), &mb); err != nil {
		t.Fatalf("not JSON: %q", b)
	}
	return reflect.DeepEqual(ma, mb)
}

func TestContract_EmitterProducesCanonicalWire(t *testing.T) {
	for _, c := range canonicalContract {
		var buf bytes.Buffer
		c.emit(NewEmitter(&buf))
		got := strings.TrimSpace(buf.String())
		if !jsonEqual(t, got, c.line) {
			t.Fatalf("contract drift:\n  emitted:  %s\n  contract: %s", got, c.line)
		}
		// And the line must be self-describing (a `type` string) — the daemon
		// dispatches on it.
		var m map[string]any
		_ = json.Unmarshal([]byte(c.line), &m)
		if _, ok := m["type"].(string); !ok {
			t.Fatalf("contract line lacks a string type: %s", c.line)
		}
	}
}
