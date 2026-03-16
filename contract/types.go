package contract

import (
	"context"
	"encoding/json"
)

// Message represents a conversation message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall represents a tool invocation requested by the LLM.
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"`
}

// ToolResult represents the result of a tool invocation.
type ToolResult struct {
	CallID   string `json:"call_id"`
	Content  string `json:"content"`
	IsError  bool   `json:"is_error,omitempty"`
	ToolName string `json:"tool_name,omitempty"` // for post-hook matching
}

// AsMessage converts a ToolResult to a Message for conversation history.
func (r *ToolResult) AsMessage() Message {
	return Message{
		Role:       "tool",
		Content:    r.Content,
		ToolCallID: r.CallID,
	}
}

// Usage tracks token consumption.
type Usage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd,omitempty"` // provider-computed cost
}

// StreamChunk represents a piece of streaming output.
type StreamChunk struct {
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Done      bool       `json:"done,omitempty"`
	Usage     *Usage     `json:"usage,omitempty"`
}

// EffortLevel controls LLM reasoning depth / token budget.
type EffortLevel string

const (
	EffortLow    EffortLevel = "low"    // fast, cheap — Guard steps, simple routing
	EffortMedium EffortLevel = "medium" // default
	EffortHigh   EffortLevel = "high"   // deep reasoning — core agent work
)

// LLM abstracts any large language model provider.
type LLM interface {
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	Stream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error)
}

// ChatRequest is the input to an LLM call.
type ChatRequest struct {
	Model       string           `json:"model"`
	Messages    []Message        `json:"messages"`
	Tools       []ToolDef        `json:"tools,omitempty"`
	MaxTokens   int              `json:"max_tokens,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"`
	Schema      *json.RawMessage `json:"schema,omitempty"`
	Effort      EffortLevel      `json:"effort,omitempty"` // v1.4
}

// ChatResponse is the output of an LLM call.
type ChatResponse struct {
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	Usage      Usage      `json:"usage"`
	StopReason string     `json:"stop_reason"`
}

// AsMessage converts a ChatResponse to a Message for conversation history.
func (r *ChatResponse) AsMessage() Message {
	return Message{
		Role:      "assistant",
		Content:   r.Content,
		ToolCalls: r.ToolCalls,
	}
}

// ToolDispatcher routes tool calls to their implementations.
type ToolDispatcher interface {
	ListTools(ctx context.Context) ([]ToolDef, error)
	Dispatch(ctx context.Context, call ToolCall) (*ToolResult, error)
}

// ToolDef describes a tool available to the LLM.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	ReadOnly    bool            `json:"read_only,omitempty"` // v1.4: parallel dispatch hint
}

// ToolHook intercepts individual tool calls within a ToolLoop.
type ToolHook struct {
	// Matcher selects which tools this hook applies to. nil = all tools.
	Matcher func(toolName string) bool

	// Pre runs before dispatch. Can modify the call or return error to block.
	Pre func(ctx context.Context, call ToolCall) (ToolCall, error)

	// Post runs after dispatch. Can inspect/log result or return error to abort.
	Post func(ctx context.Context, call ToolCall, result *ToolResult) error
}

// StreamSink receives streaming output from steps.
type StreamSink interface {
	SendChunk(chunk StreamChunk) error
	SendEvent(eventType string, data any) error
	Close() error
}
