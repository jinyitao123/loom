package main

import (
	"encoding/json"
	"io"
	"sync"
)

// Emitter writes the loom-run stdout-json (NDJSON) event stream consumed by the
// weave daemon's LoomBackend (src/cli/daemon/agent/loom.ts parseLine). One JSON
// object per line. The field names here are the contract — keep them in lockstep
// with the TypeScript parseLine. See weave plans/loom-cli-design.md.
//
// Event vocabulary:
//
//	{"type":"session_init","session_id":"..."}   emit early (gates agent_started)
//	{"type":"text","text":"..."}
//	{"type":"thinking","text":"..."}
//	{"type":"tool_use","id":"...","name":"...","input":{...}}
//	{"type":"tool_result","id":"...","output":"...","status":"success"|"error"}
//	{"type":"turn_end","session_id":"..."}
//	{"type":"error","message":"..."}
//	{"type":"result","status":"completed"|"failed"|"timeout","output":"...","session_id":"..."}
type Emitter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewEmitter returns an Emitter that writes NDJSON to w (normally os.Stdout).
func NewEmitter(w io.Writer) *Emitter { return &Emitter{w: w} }

func (e *Emitter) emit(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	// Serialize writes: tool hooks may run from parallel dispatch goroutines.
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err = e.w.Write(b)
	return err
}

type sessionInitEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
}

type textEvent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolUseEvent struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

type toolResultEvent struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Output string `json:"output"`
	Status string `json:"status"`
}

type turnEndEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
}

type errorEvent struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type resultEvent struct {
	Type      string `json:"type"`
	Status    string `json:"status"`
	Output    string `json:"output"`
	SessionID string `json:"session_id"`
}

// SessionInit announces the session/run id. Emit this first so the daemon can
// resolve its sessionId promise (it gates agent_started + steering on it).
func (e *Emitter) SessionInit(id string) error {
	return e.emit(sessionInitEvent{Type: "session_init", SessionID: id})
}

// Text emits an assistant text chunk (or the whole message in the non-streaming
// first version).
func (e *Emitter) Text(text string) error {
	return e.emit(textEvent{Type: "text", Text: text})
}

// Thinking emits a reasoning/thinking chunk.
func (e *Emitter) Thinking(text string) error {
	return e.emit(textEvent{Type: "thinking", Text: text})
}

// ToolUse emits a tool invocation. input may be nil (field omitted).
func (e *Emitter) ToolUse(id, name string, input json.RawMessage) error {
	return e.emit(toolUseEvent{Type: "tool_use", ID: id, Name: name, Input: input})
}

// ToolResult emits a tool result. status is "success" or "error".
func (e *Emitter) ToolResult(id, output, status string) error {
	return e.emit(toolResultEvent{Type: "tool_result", ID: id, Output: output, Status: status})
}

// TurnEnd marks the end of a turn. sessionID is optional.
func (e *Emitter) TurnEnd(sessionID string) error {
	return e.emit(turnEndEvent{Type: "turn_end", SessionID: sessionID})
}

// Error emits a mid-stream error (the daemon also persists it as an error
// message). Usually followed by a Result with status "failed".
func (e *Emitter) Error(message string) error {
	return e.emit(errorEvent{Type: "error", Message: message})
}

// Result is the terminal event: it resolves the daemon's result promise and
// settles the task (completeTask / failTask).
func (e *Emitter) Result(status, output, sessionID string) error {
	return e.emit(resultEvent{Type: "result", Status: status, Output: output, SessionID: sessionID})
}
