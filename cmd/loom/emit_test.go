package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// parseLines splits NDJSON output into one decoded object per line.
func parseLines(t *testing.T, b *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line is not valid JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestEmitSessionInit(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf)
	if err := e.SessionInit("sess_1"); err != nil {
		t.Fatal(err)
	}
	lines := parseLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	if lines[0]["type"] != "session_init" || lines[0]["session_id"] != "sess_1" {
		t.Fatalf("bad session_init: %v", lines[0])
	}
}

func TestEmitText(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf)
	e.Text("hello world")
	lines := parseLines(t, &buf)
	if lines[0]["type"] != "text" || lines[0]["text"] != "hello world" {
		t.Fatalf("bad text: %v", lines[0])
	}
}

func TestEmitThinking(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf)
	e.Thinking("let me think")
	lines := parseLines(t, &buf)
	if lines[0]["type"] != "thinking" || lines[0]["text"] != "let me think" {
		t.Fatalf("bad thinking: %v", lines[0])
	}
}

func TestEmitToolUse(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf)
	e.ToolUse("t1", "send_email", json.RawMessage(`{"to":"a@b.c"}`))
	lines := parseLines(t, &buf)
	got := lines[0]
	if got["type"] != "tool_use" || got["id"] != "t1" || got["name"] != "send_email" {
		t.Fatalf("bad tool_use header: %v", got)
	}
	input, ok := got["input"].(map[string]any)
	if !ok || input["to"] != "a@b.c" {
		t.Fatalf("bad tool_use input: %v", got["input"])
	}
}

func TestEmitToolUseNilInputOmitsField(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf)
	e.ToolUse("t1", "noop", nil)
	lines := parseLines(t, &buf)
	if _, present := lines[0]["input"]; present {
		t.Fatalf("input should be omitted when nil: %v", lines[0])
	}
}

func TestEmitToolResult(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf)
	e.ToolResult("t1", "ok done", "success")
	lines := parseLines(t, &buf)
	got := lines[0]
	if got["type"] != "tool_result" || got["id"] != "t1" || got["output"] != "ok done" || got["status"] != "success" {
		t.Fatalf("bad tool_result: %v", got)
	}
}

func TestEmitTurnEndOmitsEmptySession(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf)
	e.TurnEnd("")
	lines := parseLines(t, &buf)
	if lines[0]["type"] != "turn_end" {
		t.Fatalf("bad turn_end: %v", lines[0])
	}
	if _, present := lines[0]["session_id"]; present {
		t.Fatalf("session_id should be omitted when empty: %v", lines[0])
	}
}

func TestEmitTurnEndWithSession(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf)
	e.TurnEnd("sess_9")
	lines := parseLines(t, &buf)
	if lines[0]["session_id"] != "sess_9" {
		t.Fatalf("want session_id sess_9: %v", lines[0])
	}
}

func TestEmitError(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf)
	e.Error("boom")
	lines := parseLines(t, &buf)
	if lines[0]["type"] != "error" || lines[0]["message"] != "boom" {
		t.Fatalf("bad error: %v", lines[0])
	}
}

func TestEmitResult(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf)
	e.Result("completed", "final answer", "sess_1")
	lines := parseLines(t, &buf)
	got := lines[0]
	if got["type"] != "result" || got["status"] != "completed" || got["output"] != "final answer" || got["session_id"] != "sess_1" {
		t.Fatalf("bad result: %v", got)
	}
}

func TestEmitNDJSONOnePerLine(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf)
	e.SessionInit("s")
	e.Text("a")
	e.Result("completed", "a", "s")
	lines := parseLines(t, &buf)
	if len(lines) != 3 {
		t.Fatalf("want 3 NDJSON lines, got %d: %q", len(lines), buf.String())
	}
	if lines[0]["type"] != "session_init" || lines[1]["type"] != "text" || lines[2]["type"] != "result" {
		t.Fatalf("wrong event order: %v", lines)
	}
}
