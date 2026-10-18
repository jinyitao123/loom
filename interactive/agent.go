package interactive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

// LLMFactory builds the model client for one configured model.
type LLMFactory func(model Model, apiKey string) (contract.LLM, error)

// Options configures an Agent.
type Options struct {
	Cwd              string                  // working directory of the agent and its tools
	SessionDir       string                  // directory that receives new session files
	Catalog          *Catalog                // configured models and keys
	NewLLM           LLMFactory              // model client factory
	Tools            contract.ToolDispatcher // tools offered to the model
	SystemPrompt     string                  // system prompt for every model request
	MaxIterations    int                     // model rounds per prompt; <=0 means 500
	ReserveTokens    int                     // compaction runs when context exceeds window minus this; <=0 means 16384
	KeepRecentTokens int                     // recent history kept verbatim by compaction; <=0 means 20000
	Skills           []Skill                 // skills offered by "/skill:<name>" (also listed in SystemPrompt by the caller)
	Emit             func(event any)         // receives every protocol event; must be safe for concurrent use
	// OnTurnStart runs when a turn begins, before the model sees the
	// conversation; capability servers use it to bind the turn's prompt. An
	// error is reported as an extension_error event and the turn continues.
	OnTurnStart func(ctx context.Context, prompt string) error
}

// queued is a prompt waiting to be delivered.
type queued struct {
	text   string
	images []Block
}

// ThinkingLevels lists the accepted thinking levels.
var ThinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// ErrBusy reports a prompt sent while the agent is already working.
var ErrBusy = errors.New("agent is already working; send it as steer or follow_up")

// ErrNoModel reports a prompt with no model selected.
var ErrNoModel = errors.New("no model selected")

// Agent is one interactive session: it accepts prompts, runs them through a
// stdlib tool loop one at a time, streams events and appends every message to
// the session file as it completes.
// Agent 是一个交互会话：逐条接收提问，交给 stdlib 工具循环执行，
// 流式发出事件，并在每条消息完成时追加到会话文件。
type Agent struct {
	opts Options

	mu             sync.Mutex
	session        *Session
	model          *Model
	thinking       string
	running        bool
	compacting     bool
	cancel         context.CancelFunc
	idle           chan struct{}
	steering       []queued
	followUp       []queued
	autoCompaction bool
	autoRetry      bool
	steeringMode   string
	followUpMode   string
	retryCancel    chan struct{} // closed by AbortRetry while a retry waits
	currentRun     *promptRun    // the prompt in progress, for tool progress
}

// New opens sessionPath (or starts a new session when empty) and selects the
// model: the explicit provider/model, else the session's last model, else the
// first configured model.
func New(opts Options, sessionPath, provider, modelID, thinking string) (*Agent, error) {
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = 500
	}
	if opts.ReserveTokens <= 0 {
		opts.ReserveTokens = DefaultReserveTokens
	}
	if opts.KeepRecentTokens <= 0 {
		opts.KeepRecentTokens = DefaultKeepRecentTokens
	}
	if opts.Catalog == nil {
		opts.Catalog = &Catalog{keys: map[string]string{}}
	}
	a := &Agent{opts: opts, thinking: "medium", autoCompaction: true, autoRetry: true, steeringMode: "one-at-a-time", followUpMode: "one-at-a-time"}
	a.idle = make(chan struct{})
	close(a.idle)
	if sessionPath != "" {
		s, err := OpenSession(sessionPath)
		if err != nil {
			return nil, err
		}
		a.session = s
	} else {
		a.session = NewSession(opts.SessionDir, opts.Cwd, "")
	}
	a.restoreSettings()
	if modelID != "" {
		m, ok := opts.Catalog.Find(provider, modelID)
		if !ok {
			return nil, fmt.Errorf("model not found: %s/%s", provider, modelID)
		}
		a.model = &m
	}
	if thinking != "" {
		if !validThinking(thinking) {
			return nil, fmt.Errorf("invalid thinking level %q", thinking)
		}
		a.thinking = thinking
	}
	return a, nil
}

// restoreSettings picks the model and thinking level recorded in the session.
func (a *Agent) restoreSettings() {
	ctx := a.session.Context()
	a.model = nil
	if ctx.ModelID != "" {
		if m, ok := a.opts.Catalog.Find(ctx.Provider, ctx.ModelID); ok {
			a.model = &m
		}
	}
	if a.model == nil && len(a.opts.Catalog.Models) > 0 {
		m := a.opts.Catalog.Models[0]
		a.model = &m
	}
	if ctx.ThinkingLevel != "" {
		a.thinking = ctx.ThinkingLevel
	}
}

func validThinking(level string) bool {
	for _, l := range ThinkingLevels {
		if l == level {
			return true
		}
	}
	return false
}

func (a *Agent) emit(event map[string]any) {
	if a.opts.Emit != nil {
		a.opts.Emit(event)
	}
}

// State reports the session state.
func (a *Agent) State() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := map[string]any{
		"model":                 a.model,
		"thinkingLevel":         a.thinking,
		"isStreaming":           a.running,
		"isCompacting":          a.compacting,
		"steeringMode":          a.steeringMode,
		"followUpMode":          a.followUpMode,
		"sessionFile":           a.session.Path(),
		"sessionId":             a.session.ID(),
		"autoCompactionEnabled": a.autoCompaction,
		"messageCount":          len(a.session.Context().Messages),
		"pendingMessageCount":   len(a.steering) + len(a.followUp),
	}
	if name := a.session.Name(); name != "" {
		state["sessionName"] = name
	}
	return state
}

// Prompt starts working on text and images. While the agent is working,
// behavior must be "steer" or "followUp" and the prompt is queued instead.
func (a *Agent) Prompt(text string, images []Block, behavior string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	item := queued{text: text, images: images}
	if a.running {
		switch behavior {
		case "steer":
			a.enqueueLocked(&a.steering, item)
			return nil
		case "followUp":
			a.enqueueLocked(&a.followUp, item)
			return nil
		}
		return ErrBusy
	}
	if a.model == nil {
		return ErrNoModel
	}
	a.startLocked(item)
	return nil
}

// Steer queues a message for the running prompt; it is delivered after the
// current model round's tool calls finish, before the next model request.
// Steer 为正在执行的提问排队一条插话，在本轮工具调用结束后、下一次请求模型前送达。
func (a *Agent) Steer(text string, images []Block) error {
	return a.queue(&a.steering, queued{text: text, images: images})
}

// FollowUp queues a message to run after the current prompt finishes.
func (a *Agent) FollowUp(text string, images []Block) error {
	return a.queue(&a.followUp, queued{text: text, images: images})
}

func (a *Agent) queue(target *[]queued, item queued) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running {
		if a.model == nil {
			return ErrNoModel
		}
		a.startLocked(item)
		return nil
	}
	a.enqueueLocked(target, item)
	return nil
}

func (a *Agent) enqueueLocked(target *[]queued, item queued) {
	*target = append(*target, item)
	a.emitQueueLocked()
}

func queueTexts(items []queued) []string {
	texts := make([]string, len(items))
	for i, item := range items {
		texts[i] = item.text
	}
	return texts
}

func (a *Agent) emitQueueLocked() {
	a.emit(map[string]any{"type": "queue_update", "steering": queueTexts(a.steering), "followUp": queueTexts(a.followUp)})
}

// takeLocked removes the next queued messages: one, or all when mode is "all".
func takeLocked(queue *[]queued, mode string) []queued {
	if len(*queue) == 0 {
		return nil
	}
	n := 1
	if mode == "all" {
		n = len(*queue)
	}
	taken := append([]queued(nil), (*queue)[:n]...)
	*queue = (*queue)[n:]
	return taken
}

// startJob marks the agent busy and runs job in the background until it
// returns; Abort cancels its context.
func (a *Agent) startJobLocked(job func(ctx context.Context)) {
	ctx, cancel := context.WithCancel(context.Background())
	a.running = true
	a.cancel = cancel
	idle := make(chan struct{})
	a.idle = idle
	go func() {
		defer close(idle)
		defer cancel()
		job(ctx)
	}()
}

func (a *Agent) startLocked(first queued) {
	a.startJobLocked(func(ctx context.Context) {
		next := []queued{first}
		for {
			a.runPrompt(ctx, next)
			a.mu.Lock()
			if ctx.Err() != nil || len(a.steering)+len(a.followUp) == 0 {
				a.running = false
				a.mu.Unlock()
				a.emit(map[string]any{"type": "agent_settled"})
				return
			}
			if next = takeLocked(&a.steering, a.steeringMode); next == nil {
				next = takeLocked(&a.followUp, a.followUpMode)
			}
			a.emitQueueLocked()
			a.mu.Unlock()
		}
	})
}

// Compact summarizes older history now. It fails while a prompt is running.
func (a *Agent) Compact(instructions string) (*CompactionResult, error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return nil, ErrBusy
	}
	if a.model == nil {
		a.mu.Unlock()
		return nil, ErrNoModel
	}
	var result *CompactionResult
	var err error
	a.compacting = true
	a.startJobLocked(func(ctx context.Context) {
		result, err = a.compact(ctx, "manual", instructions, false)
		a.mu.Lock()
		a.running, a.compacting = false, false
		a.mu.Unlock()
	})
	idle := a.idle
	a.mu.Unlock()
	<-idle
	return result, err
}

// Abort stops the running prompt, drops queued input and waits until idle.
func (a *Agent) Abort() {
	a.mu.Lock()
	cancel, idle := a.cancel, a.idle
	if len(a.steering)+len(a.followUp) > 0 {
		a.steering, a.followUp = nil, nil
		a.emitQueueLocked()
	}
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	<-idle
}

// NewSession aborts any running prompt and starts a fresh, unsaved session.
func (a *Agent) NewSession(parentSession string) {
	a.Abort()
	a.mu.Lock()
	defer a.mu.Unlock()
	model := a.model
	a.session = NewSession(a.opts.SessionDir, a.opts.Cwd, parentSession)
	a.model = model
}

// SwitchSession aborts any running prompt and opens another session file.
func (a *Agent) SwitchSession(path string) error {
	s, err := OpenSession(path)
	if err != nil {
		return err
	}
	a.Abort()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.session = s
	a.restoreSettings()
	return nil
}

// Messages returns the active conversation.
func (a *Agent) Messages() []Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.session.Context().Messages
}

// Models returns the configured models.
func (a *Agent) Models() []Model { return a.opts.Catalog.Models }

// SetModel switches the model and records the change in the session.
func (a *Agent) SetModel(provider, id string) (Model, error) {
	m, ok := a.opts.Catalog.Find(provider, id)
	if !ok {
		return Model{}, fmt.Errorf("model not found: %s/%s", provider, id)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.model = &m
	return m, a.session.Append(&Entry{Type: "model_change", Provider: m.Provider, ModelID: m.ID})
}

// SetThinkingLevel changes the thinking level and records it in the session.
func (a *Agent) SetThinkingLevel(level string) error {
	if !validThinking(level) {
		return fmt.Errorf("invalid thinking level %q", level)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.thinking = level
	return a.session.Append(&Entry{Type: "thinking_level_change", ThinkingLevel: level})
}

// SetSessionName records a display name for the session.
func (a *Agent) SetSessionName(name string) error {
	name = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(name))
	if name == "" {
		return errors.New("session name is empty")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.session.Append(&Entry{Type: "session_info", Name: &name})
}

// SetQueueModes records the steering and follow-up delivery modes.
func (a *Agent) SetQueueModes(steering, followUp string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if steering != "" {
		a.steeringMode = steering
	}
	if followUp != "" {
		a.followUpMode = followUp
	}
}

// SetAutoCompaction and SetAutoRetry record the host's preference.
func (a *Agent) SetAutoCompaction(enabled bool) {
	a.mu.Lock()
	a.autoCompaction = enabled
	a.mu.Unlock()
}
func (a *Agent) SetAutoRetry(enabled bool) { a.mu.Lock(); a.autoRetry = enabled; a.mu.Unlock() }

// LastAssistantText returns the text of the last assistant message, if any.
func (a *Agent) LastAssistantText() *string {
	messages := a.Messages()
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			text := messages[i].Text()
			return &text
		}
	}
	return nil
}

// ForkMessages lists the user messages of the active branch.
func (a *Agent) ForkMessages() []map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []map[string]string
	for _, entry := range a.session.branch() {
		if entry.Type == "message" && entry.Message != nil && entry.Message.Role == "user" {
			out = append(out, map[string]string{"entryId": entry.ID, "text": entry.Message.Text()})
		}
	}
	return out
}

// Stats summarizes token use and cost across the whole session file.
func (a *Agent) Stats() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	var users, assistants, calls, results, total int
	var tokens Usage
	var last *Usage
	for _, entry := range a.session.Entries() {
		if entry.Type == "compaction" {
			if u := entry.Usage; u != nil {
				tokens.Input += u.Input
				tokens.Output += u.Output
				tokens.Cost.Total += u.Cost.Total
			}
			last = nil // context size is unknown until the next answer
			continue
		}
		if entry.Type != "message" || entry.Message == nil {
			continue
		}
		m := entry.Message
		total++
		switch m.Role {
		case "user":
			users++
		case "assistant":
			assistants++
			for _, b := range m.Content {
				if b.Type == "toolCall" {
					calls++
				}
			}
			if m.Usage != nil {
				tokens.Input += m.Usage.Input
				tokens.Output += m.Usage.Output
				tokens.CacheRead += m.Usage.CacheRead
				tokens.CacheWrite += m.Usage.CacheWrite
				tokens.Cost.Total += m.Usage.Cost.Total
				last = m.Usage
			}
		case "toolResult":
			results++
		}
	}
	stats := map[string]any{
		"sessionFile":       a.session.Path(),
		"sessionId":         a.session.ID(),
		"userMessages":      users,
		"assistantMessages": assistants,
		"toolCalls":         calls,
		"toolResults":       results,
		"totalMessages":     total,
		"tokens": map[string]int{
			"input": tokens.Input, "output": tokens.Output, "cacheRead": tokens.CacheRead, "cacheWrite": tokens.CacheWrite,
			"total": tokens.Input + tokens.Output + tokens.CacheRead + tokens.CacheWrite,
		},
		"cost": tokens.Cost.Total,
	}
	if a.model != nil && a.model.ContextWindow > 0 {
		usage := map[string]any{"contextWindow": a.model.ContextWindow, "tokens": nil, "percent": nil}
		if last != nil {
			used := last.Input + last.Output + last.CacheRead + last.CacheWrite
			usage["tokens"] = used
			usage["percent"] = float64(used) * 100 / float64(a.model.ContextWindow)
		}
		stats["contextUsage"] = usage
	}
	return stats
}

// run state shared by the model wrapper and the tool hooks of one prompt.
type promptRun struct {
	agent       *Agent
	model       Model
	thinking    string
	added       []Message
	assistant   *Message
	toolResults []Message
	turnHasTool bool
	lastErr     error

	toolMu           sync.Mutex
	tools            map[string]toolCallInfo // running tool calls by id, for progress events
	compactionFailed bool                    // threshold compaction failed; not retried this turn
}

// toolCallInfo is what a progress event repeats about its tool call.
type toolCallInfo struct {
	name string
	args json.RawMessage
}

func (a *Agent) setCurrentRun(run *promptRun) {
	a.mu.Lock()
	a.currentRun = run
	a.mu.Unlock()
}

// ToolProgress reports partial output of a running tool call as a
// tool_execution_update event; output is the whole output so far. Calls that
// are not running in the current prompt are ignored.
// ToolProgress 以 tool_execution_update 事件报告运行中工具调用的阶段输出（迄今全部输出）；
// 不属于当前提问的调用忽略。
func (a *Agent) ToolProgress(callID, output string) {
	a.mu.Lock()
	run := a.currentRun
	a.mu.Unlock()
	if run == nil {
		return
	}
	run.toolMu.Lock()
	info, ok := run.tools[callID]
	run.toolMu.Unlock()
	if !ok {
		return
	}
	a.emit(map[string]any{"type": "tool_execution_update", "toolCallId": callID, "toolName": info.name, "args": info.args,
		"partialResult": map[string]any{"content": []Block{{Type: "text", Text: output}}, "details": map[string]any{}}})
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// append persists a message and emits its start and end events.
func (r *promptRun) append(m Message) {
	r.agent.mu.Lock()
	err := r.agent.session.Append(&Entry{Type: "message", Message: &m})
	r.agent.mu.Unlock()
	if err != nil {
		r.agent.emit(map[string]any{"type": "extension_error", "extensionPath": "session", "event": "persist", "error": err.Error()})
	}
	r.added = append(r.added, m)
}

func (r *promptRun) appendAndAnnounce(m Message) {
	r.agent.emit(map[string]any{"type": "message_start", "message": m})
	r.append(m)
	r.agent.emit(map[string]any{"type": "message_end", "message": m})
}

// userMessage builds the stored user message of a queued prompt, expanding a
// "/skill:<name>" command.
func (a *Agent) userMessage(item queued) Message {
	text, err := expandSkillCommand(item.text, a.opts.Skills)
	if err != nil {
		text = item.text
		a.emit(map[string]any{"type": "extension_error", "extensionPath": "skills", "event": "expand", "error": err.Error()})
	}
	content := textBlocks(text)
	content = append(content, fitImageBlocks(item.images)...)
	return Message{Role: "user", Content: content, Timestamp: nowMillis()}
}

// loopMessages rebuilds the transcript of the running loop from the session,
// keeping the system message the loop prepended.
func (a *Agent) loopMessages(current []contract.Message, target Model) []contract.Message {
	a.mu.Lock()
	history := toContract(a.session.Context().Messages, target)
	a.mu.Unlock()
	if len(current) > 0 && current[0].Role == "system" {
		return append([]contract.Message{current[0]}, history...)
	}
	return history
}

// needsCompaction reports whether the active context exceeds the threshold.
func (a *Agent) needsCompaction() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.autoCompaction || a.model == nil {
		return false
	}
	threshold := compactionThreshold(a.model.ContextWindow, a.opts.ReserveTokens)
	return threshold > 0 && contextTokens(a.session.Context().Messages) > threshold
}

func (a *Agent) autoCompactionEnabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.autoCompaction
}

func nonNil(messages []Message) []Message {
	if messages == nil {
		return []Message{}
	}
	return messages
}

// toolHook announces each tool call and records its result in the session.
func (r *promptRun) toolHook() contract.ToolHook {
	return contract.ToolHook{
		Pre: func(_ context.Context, call contract.ToolCall) (contract.ToolCall, error) {
			args := rawArgs(call.Args)
			r.toolMu.Lock()
			r.tools[call.ID] = toolCallInfo{name: call.Name, args: args}
			r.toolMu.Unlock()
			r.agent.emit(map[string]any{"type": "tool_execution_start", "toolCallId": call.ID, "toolName": call.Name, "args": args})
			return call, nil
		},
		Post: func(_ context.Context, call contract.ToolCall, result *contract.ToolResult) error {
			r.toolMu.Lock()
			delete(r.tools, call.ID)
			r.toolMu.Unlock()
			fitResultImages(result)
			content := resultBlocks(result)
			r.agent.emit(map[string]any{"type": "tool_execution_end", "toolCallId": call.ID, "toolName": call.Name,
				"result": map[string]any{"content": content, "details": map[string]any{}}, "isError": result.IsError})
			m := Message{Role: "toolResult", ToolCallID: call.ID, ToolName: call.Name, Content: content, IsError: boolPtr(result.IsError), Timestamp: nowMillis()}
			r.appendAndAnnounce(m)
			r.toolResults = append(r.toolResults, m)
			return nil
		},
	}
}

// fitResultImages scales a tool result's images down in place, before the
// result reaches the model and the session.
func fitResultImages(result *contract.ToolResult) {
	has := false
	for _, p := range result.Parts {
		has = has || p.Type == "image"
	}
	if !has {
		return
	}
	var blocks []Block
	for _, p := range result.Parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, Block{Type: "text", Text: p.Text})
		case "image":
			blocks = append(blocks, Block{Type: "image", Data: p.Data, MimeType: p.MediaType})
		}
	}
	parts := make([]contract.ContentPart, 0, len(blocks))
	for _, b := range fitImageBlocks(blocks) {
		if b.Type == "image" {
			parts = append(parts, contract.ContentPart{Type: "image", Data: b.Data, MediaType: b.MimeType})
		} else {
			parts = append(parts, contract.ContentPart{Type: "text", Text: b.Text})
		}
	}
	result.Parts = parts
}

// resultBlocks converts a tool result to content blocks, keeping its images.
func resultBlocks(result *contract.ToolResult) []Block {
	if len(result.Parts) == 0 {
		if blocks := textBlocks(result.Content); blocks != nil {
			return blocks
		}
		return []Block{}
	}
	blocks := make([]Block, 0, len(result.Parts))
	for _, p := range result.Parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, Block{Type: "text", Text: p.Text})
		case "image":
			blocks = append(blocks, Block{Type: "image", Data: p.Data, MimeType: p.MediaType})
		}
	}
	return blocks
}

func rawArgs(args string) json.RawMessage {
	if json.Valid([]byte(args)) && strings.TrimSpace(args) != "" {
		return json.RawMessage(args)
	}
	return json.RawMessage("{}")
}

// observedLLM turns each model round of the tool loop into turn and message
// events, streams text deltas, and persists the finished assistant message.
// observedLLM 把工具循环的每一轮模型调用转成轮次与消息事件，流式转发文本增量，并持久化完整的助手消息。
type observedLLM struct {
	inner contract.LLM
	run   *promptRun
}

func (o *observedLLM) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return o.inner.Stream(ctx, req)
}

// Chat runs one model round. Transient provider failures (rate limits,
// server errors, dropped streams) are retried with backoff while automatic
// retry is on; the failed attempt is shown but not stored.
// Chat 执行一轮模型调用；自动重试开启时，限流、服务端错误、流中断等暂时性失败按退避重试，
// 失败的那次只显示、不入会话。
func (o *observedLLM) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := o.run
	a := r.agent
	if r.turnHasTool {
		a.emit(map[string]any{"type": "turn_end", "message": r.assistant, "toolResults": nonNil(r.toolResults)})
		a.emit(map[string]any{"type": "turn_start"})
		r.turnHasTool, r.toolResults = false, nil
	}
	req.Effort = effortFor(r.model, r.thinking)

	attempts := 0 // retries announced so far
	for retry := 0; ; retry++ {
		resp, final, err := o.attempt(ctx, req)
		if err != nil && ctx.Err() == nil && retry < maxRetries && IsTransient(err) && a.autoRetryEnabled() {
			a.emit(map[string]any{"type": "message_end", "message": final})
			delay := retryDelay(retry)
			attempts = retry + 1
			cancel := a.armRetry()
			a.emit(map[string]any{"type": "auto_retry_start", "attempt": attempts, "maxAttempts": maxRetries, "delayMs": delay.Milliseconds(), "errorMessage": err.Error()})
			if a.waitForRetry(ctx, cancel, delay) {
				continue
			}
		}
		if attempts > 0 {
			if err == nil {
				a.emit(map[string]any{"type": "auto_retry_end", "success": true, "attempt": attempts})
			} else {
				a.emit(map[string]any{"type": "auto_retry_end", "success": false, "attempt": attempts, "finalError": err.Error()})
			}
		}
		if err != nil && ctx.Err() == nil {
			r.lastErr = err
		}
		r.append(final)
		a.emit(map[string]any{"type": "message_end", "message": final})
		r.assistant = &final
		r.turnHasTool = err == nil && len(resp.ToolCalls) > 0
		return resp, err
	}
}

// attempt streams one model request and returns the response with the
// assistant message it produced; the message is not stored yet.
func (o *observedLLM) attempt(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, Message, error) {
	r := o.run
	a := r.agent
	partial := Message{Role: "assistant", Content: []Block{}, API: r.model.API, Provider: r.model.Provider, Model: r.model.ID, Usage: &Usage{}, Timestamp: nowMillis()}
	a.emit(map[string]any{"type": "message_start", "message": partial})
	sink := &deltaSink{agent: a}
	resp, err := stdlib.NewStreamingLLM(o.inner, sink).Chat(ctx, req)
	sink.close()

	final := partial
	if err != nil {
		if thought := sink.thinking.String(); thought != "" {
			final.Content = append(final.Content, Block{Type: "thinking", Thinking: thought})
		}
		final.Content = append(final.Content, textBlocks(sink.text.String())...)
		if ctx.Err() != nil {
			final.StopReason = "aborted"
		} else {
			final.StopReason = "error"
			final.ErrorMessage = err.Error()
		}
	} else {
		// The controlled tool loop reads stop reasons strictly; providers
		// differ, so they are normalized here: tool calls, length, or stop.
		switch {
		case len(resp.ToolCalls) > 0:
			resp.StopReason = "tool_calls"
		case resp.StopReason != "length":
			resp.StopReason = "stop"
		}
		final.Content = thinkingBlocks(resp.Reasoning)
		final.Content = append(final.Content, textBlocks(resp.Content)...)
		for _, call := range resp.ToolCalls {
			final.Content = append(final.Content, Block{Type: "toolCall", ID: call.ID, Name: call.Name, Arguments: rawArgs(call.Args)})
		}
		final.Usage = usageFor(r.model, resp.Usage)
		final.StopReason = stopReason(resp)
	}
	if final.Content == nil {
		final.Content = []Block{}
	}
	return resp, final, err
}

func effortFor(model Model, thinking string) contract.EffortLevel {
	if !model.Reasoning {
		return ""
	}
	switch thinking {
	case "off":
		return ""
	case "minimal", "low":
		return contract.EffortLow
	case "high", "xhigh", "max":
		return contract.EffortHigh
	default:
		return contract.EffortMedium
	}
}

func stopReason(resp *contract.ChatResponse) string {
	if len(resp.ToolCalls) > 0 {
		return "toolUse"
	}
	if resp.StopReason == "length" {
		return "length"
	}
	return "stop"
}

func usageFor(model Model, u contract.Usage) *Usage {
	// The contract's input includes the cached parts; the session format
	// counts them separately.
	input := u.InputTokens - u.CacheReadTokens - u.CacheWriteTokens
	if input < 0 {
		input = 0
	}
	usage := &Usage{Input: input, Output: u.OutputTokens, CacheRead: u.CacheReadTokens, CacheWrite: u.CacheWriteTokens,
		TotalTokens: input + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens}
	usage.Cost.Input = float64(input) * model.Cost.Input / 1e6
	usage.Cost.Output = float64(u.OutputTokens) * model.Cost.Output / 1e6
	usage.Cost.CacheRead = float64(u.CacheReadTokens) * model.Cost.CacheRead / 1e6
	usage.Cost.CacheWrite = float64(u.CacheWriteTokens) * model.Cost.CacheWrite / 1e6
	usage.Cost.Total = usage.Cost.Input + usage.Cost.Output + usage.Cost.CacheRead + usage.Cost.CacheWrite
	if u.CostUSD > 0 {
		usage.Cost.Total = u.CostUSD
	}
	return usage
}

// deltaSink forwards thinking and text deltas as message_update events. Each
// run of one kind is a content block, thinking_start/delta/end or
// text_start/delta/end, numbered in the order the blocks open.
type deltaSink struct {
	agent    *Agent
	text     strings.Builder // all answer text
	thinking strings.Builder // all reasoning text
	kind     string          // "text" or "thinking" while a block is open
	index    int             // content index of the open block
	block    strings.Builder // text of the open block
	opened   int             // blocks opened so far
}

func (s *deltaSink) SendChunk(chunk contract.StreamChunk) error {
	if chunk.Reasoning != "" {
		s.thinking.WriteString(chunk.Reasoning)
		s.delta("thinking", chunk.Reasoning)
	}
	if chunk.Content != "" {
		s.text.WriteString(chunk.Content)
		s.delta("text", chunk.Content)
	}
	return nil
}

func (s *deltaSink) delta(kind, text string) {
	if s.kind != kind {
		s.close()
		s.kind, s.index = kind, s.opened
		s.opened++
		s.block.Reset()
		s.update(map[string]any{"type": kind + "_start", "contentIndex": s.index})
	}
	s.block.WriteString(text)
	s.update(map[string]any{"type": kind + "_delta", "contentIndex": s.index, "delta": text})
}

// close ends the open block.
func (s *deltaSink) close() {
	if s.kind != "" {
		s.update(map[string]any{"type": s.kind + "_end", "contentIndex": s.index, "content": s.block.String()})
		s.kind = ""
	}
}

func (s *deltaSink) update(event map[string]any) {
	s.agent.emit(map[string]any{"type": "message_update", "usage": Usage{}, "assistantMessageEvent": event})
}

func (s *deltaSink) SendEvent(string, any) error { return nil }
func (s *deltaSink) Close() error                { return nil }
