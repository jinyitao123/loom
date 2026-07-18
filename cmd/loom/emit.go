package main

import (
	"encoding/json" // 事件序列化为单行 JSON
	"io"            // 输出目标的最小抽象（通常为 os.Stdout）
	"sync"          // 互斥锁：保证并发 emit 时行与行不交错
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
//
// 中文补充：Emitter 负责写出 loom run 的 stdout NDJSON 事件流——每行一个
// JSON 对象，宿主（weave 守护进程的 LoomBackend）按行解析。这里的字段名
// 就是 wire 协议本身：任何改名/增删都必须与 TypeScript 侧 parseLine 同步
// 修改，否则宿主会静默丢弃或误解事件。
type Emitter struct {
	mu sync.Mutex // 写锁：多 goroutine 并发发事件时保证每行 NDJSON 完整不交错
	w  io.Writer  // 事件输出目标（正常运行时即 os.Stdout）
}

// NewEmitter returns an Emitter that writes NDJSON to w (normally os.Stdout).
// 中文补充：构造写往 w 的 Emitter；生产路径上 w 就是 stdout，测试时可注入缓冲区。
func NewEmitter(w io.Writer) *Emitter { return &Emitter{w: w} }

// emit 是所有事件的统一出口：序列化 → 追加换行 → 加锁整行写出。
func (e *Emitter) emit(v any) error {
	// 先在锁外完成 JSON 序列化，尽量缩短临界区。
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n') // NDJSON 协议：一行一个事件，换行符即事件边界
	// Serialize writes: tool hooks may run from parallel dispatch goroutines.
	// 中文补充：工具钩子可能从并行分发的 goroutine 中发事件，必须整行原子
	// 写出，防止两行 JSON 在 stdout 上交错导致宿主按行解析失败。
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err = e.w.Write(b) // 单次 Write 写出整行（含换行），保持行的原子性
	return err
}

// 以下事件结构体与 wire 协议中的事件类型一一对应；json tag 即协议字段名，
// 必须与 TypeScript 侧 parseLine 保持一致，不可随意改动。

// sessionInitEvent：会话初始化事件，向宿主通告本轮运行的 session id。
type sessionInitEvent struct {
	Type      string `json:"type"`       // 恒为 "session_init"
	SessionID string `json:"session_id"` // 会话 id：宿主用它续接（--resume）后续轮次
}

// textEvent：assistant 文本增量；thinking 事件复用同一结构，仅 type 不同。
type textEvent struct {
	Type string `json:"type"` // "text" 或 "thinking"
	Text string `json:"text"` // 文本增量（流式）或整条消息（非流式回退）
}

// toolUseEvent：一次工具调用的开始。
type toolUseEvent struct {
	Type  string          `json:"type"`            // 恒为 "tool_use"
	ID    string          `json:"id"`              // 调用 id：与后续 tool_result 配对
	Name  string          `json:"name"`            // 工具名
	Input json.RawMessage `json:"input,omitempty"` // 原样透传的调用参数 JSON；为 nil 时整个字段省略
}

// toolResultEvent：与 tool_use 按 id 配对的工具执行结果。
type toolResultEvent struct {
	Type   string `json:"type"`   // 恒为 "tool_result"
	ID     string `json:"id"`     // 对应 tool_use 事件的调用 id
	Output string `json:"output"` // 工具输出（已文本化）
	Status string `json:"status"` // "success" 或 "error"
}

// turnEndEvent：一轮对话结束的标记。
type turnEndEvent struct {
	Type      string `json:"type"`                 // 恒为 "turn_end"
	SessionID string `json:"session_id,omitempty"` // 可选：便于宿主校验事件归属的会话
}

// errorEvent：流中途的错误通告（非终态事件）。
type errorEvent struct {
	Type    string `json:"type"`    // 恒为 "error"
	Message string `json:"message"` // 人类可读的错误描述
}

// resultEvent：终态事件，为整条事件流收尾。
type resultEvent struct {
	Type      string `json:"type"`       // 恒为 "result"
	Status    string `json:"status"`     // "completed" / "failed" / "timeout" 三态之一
	Output    string `json:"output"`     // 本轮的最终输出文本
	SessionID string `json:"session_id"` // 会话 id：宿主据此落库并支持后续续接
}

// SessionInit announces the session/run id. Emit this first so the daemon can
// resolve its sessionId promise (it gates agent_started + steering on it).
// 中文补充：必须尽早、作为首个事件发出——守护进程的 sessionId promise
// 等着它兑现；agent_started 事件与 steering（转向注入）都以其为前置条件。
func (e *Emitter) SessionInit(id string) error {
	return e.emit(sessionInitEvent{Type: "session_init", SessionID: id})
}

// Text emits an assistant text chunk (or the whole message in the non-streaming
// first version).
// 中文补充：发出 assistant 文本——流式时是一个增量分片，非流式回退时是整条消息。
func (e *Emitter) Text(text string) error {
	return e.emit(textEvent{Type: "text", Text: text})
}

// Thinking emits a reasoning/thinking chunk.
// 中文补充：发出推理/思考文本分片；复用 textEvent 结构，仅 type 标记不同。
func (e *Emitter) Thinking(text string) error {
	return e.emit(textEvent{Type: "thinking", Text: text})
}

// ToolUse emits a tool invocation. input may be nil (field omitted).
// 中文补充：发出工具调用开始事件；input 为 nil 时 wire 上省略该字段（omitempty）。
func (e *Emitter) ToolUse(id, name string, input json.RawMessage) error {
	return e.emit(toolUseEvent{Type: "tool_use", ID: id, Name: name, Input: input})
}

// ToolResult emits a tool result. status is "success" or "error".
// 中文补充：发出工具结果；id 必须与先前的 tool_use 配对，status 只有两个合法取值。
func (e *Emitter) ToolResult(id, output, status string) error {
	return e.emit(toolResultEvent{Type: "tool_result", ID: id, Output: output, Status: status})
}

// TurnEnd marks the end of a turn. sessionID is optional.
// 中文补充：标记一轮对话结束；sessionID 可为空（此时 wire 上省略该字段）。
func (e *Emitter) TurnEnd(sessionID string) error {
	return e.emit(turnEndEvent{Type: "turn_end", SessionID: sessionID})
}

// Error emits a mid-stream error (the daemon also persists it as an error
// message). Usually followed by a Result with status "failed".
// 中文补充：发出流中错误（守护进程也会将其持久化为一条错误消息）；
// 它不是终态，通常随后再发一个 status 为 "failed" 的 Result 收尾。
func (e *Emitter) Error(message string) error {
	return e.emit(errorEvent{Type: "error", Message: message})
}

// Result is the terminal event: it resolves the daemon's result promise and
// settles the task (completeTask / failTask).
// 中文补充：终态事件——兑现守护进程的 result promise 并结算任务
// （completeTask / failTask）；此事件之后不应再发出任何事件。
func (e *Emitter) Result(status, output, sessionID string) error {
	return e.emit(resultEvent{Type: "result", Status: status, Output: output, SessionID: sessionID})
}
