package contract

import (
	"context"       // 所有跨进程调用（LLM、工具）都以 context 承载超时/取消
	"encoding/json" // 工具入参 schema、结构化输出 schema 均以原始 JSON 传递，不做二次解析
)

// Message represents a conversation message.
// Message 是提供商无关的对话消息抽象：内核与各 provider 之间只用这一种消息格式，
// 由各 provider 适配层负责与自家 API 的消息结构互转。
type Message struct {
	Role       string     `json:"role"`                   // 角色："system" / "user" / "assistant" / "tool"
	Content    string     `json:"content,omitempty"`      // 文本内容；纯工具调用消息可为空
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // assistant 消息携带的工具调用请求（可多个）
	ToolCallID string     `json:"tool_call_id,omitempty"` // tool 消息回填的调用 ID，与发起方 ToolCall.ID 配对
}

// ToolCall represents a tool invocation requested by the LLM.
// ToolCall 是 LLM 发起的一次工具调用请求，提供商无关。
type ToolCall struct {
	ID   string `json:"id"`   // 本次调用的唯一 ID，用于把 ToolResult 关联回这次请求
	Name string `json:"name"` // 目标工具名，对应 ToolDef.Name
	Args string `json:"args"` // 调用参数，保持为 JSON 字符串原样透传，不在 contract 层解析
}

// ToolResult represents the result of a tool invocation.
// ToolResult 是一次工具执行的结果，将被转成 tool 消息回填进对话历史。
type ToolResult struct {
	CallID     string         `json:"call_id"`               // 对应 ToolCall.ID，让模型知道结果属于哪次调用
	Content    string         `json:"content"`               // 执行结果文本（通常是 JSON），原样交还给模型
	IsError    bool           `json:"is_error,omitempty"`    // 标记执行失败：错误也作为结果反馈给模型，而不是中断循环
	ToolName   string         `json:"tool_name,omitempty"`   // for post-hook matching
	StatePatch map[string]any `json:"state_patch,omitempty"` // optional control-plane state delta; ToolLoop policy-gates it
	// ToolName 冗余记录工具名：ToolHook 的 Post 阶段只拿到结果时仍能按工具名匹配。
	Park    bool   `json:"park,omitempty"`     // true=本次调用未执行，已被平台扣下等外部批准；Content 仅为占位说明，循环不得将其当作执行结果
	ParkRef string `json:"park_ref,omitempty"` // 平台侧不透明关联引用（如审批动作 ID），原样带进暂停现场供宿主定位
	// Park marks an unexecuted call held for external approval; ParkRef carries the platform's opaque correlation reference.
}

// AsMessage converts a ToolResult to a Message for conversation history.
// AsMessage 把工具结果转成 role="tool" 的对话消息，供工具循环追加到历史后再次请求模型。
func (r *ToolResult) AsMessage() Message {
	return Message{
		Role:       "tool",    // 固定为 tool 角色，模型据此识别这是工具回执
		Content:    r.Content, // 结果正文原样进入上下文
		ToolCallID: r.CallID,  // 回填调用 ID，与发起该调用的 assistant 消息配对
	}
}

// Usage tracks token consumption.
// Usage 记录单次 LLM 调用的 token 消耗与成本，是预算钩子（budget hook）统计的数据源。
type Usage struct {
	InputTokens  int     `json:"input_tokens"`       // 输入（prompt）token 数
	OutputTokens int     `json:"output_tokens"`      // 输出（补全）token 数
	CostUSD      float64 `json:"cost_usd,omitempty"` // provider-computed cost
	// CostUSD 由 provider 按自家价格表算好再填入：contract 层不维护任何价格知识。
}

// StreamChunk represents a piece of streaming output.
// StreamChunk 是流式输出的最小单元：一条流由若干增量块组成，以 Done=true 的终止块收尾。
type StreamChunk struct {
	Content   string     `json:"content,omitempty"`    // 本块新增的文本增量（delta），非全量
	ToolCalls []ToolCall `json:"tool_calls,omitempty"` // 本块携带的工具调用片段，由消费方自行累积
	Done      bool       `json:"done,omitempty"`       // true 表示流结束：终止块通常不带内容、只带 Usage
	Usage     *Usage     `json:"usage,omitempty"`      // 用量统计，一般仅在终止块出现；指针以区分"未上报"
}

// EffortLevel controls LLM reasoning depth / token budget.
// EffortLevel 是提供商无关的三档推理档位：随 ChatRequest 透传给 provider，
// 由各 provider 映射到自家参数（如 reasoning_effort），contract 层只定义语义。
type EffortLevel string

const (
	EffortLow    EffortLevel = "low"    // fast, cheap — Guard steps, simple routing
	EffortMedium EffortLevel = "medium" // default
	EffortHigh   EffortLevel = "high"   // deep reasoning — core agent work
	// 低档给守卫步骤/简单路由等追求快与省的场景；中档为默认；高档给核心 agent 深度推理。
)

// LLM abstracts any large language model provider.
// LLM 是对任意大模型提供商的统一抽象：内核与 stdlib 只面向这两个方法编程，
// 换 provider 不需要改动任何上层代码。
type LLM interface {
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)        // 阻塞式调用：一次请求返回完整响应
	Stream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error) // 流式调用：返回只读 chunk 通道，结束时 provider 负责关闭
}

// ChatRequest is the input to an LLM call.
// ChatRequest 是提供商无关的调用入参：所有字段按各 provider 的能力做映射，不支持的字段可忽略。
type ChatRequest struct {
	Model       string           `json:"model"`                 // 模型标识，语义由具体 provider 解释
	Messages    []Message        `json:"messages"`              // 完整对话历史（含 system/user/assistant/tool）
	Tools       []ToolDef        `json:"tools,omitempty"`       // 本次调用暴露给模型的工具清单；空表示禁用工具
	MaxTokens   int              `json:"max_tokens,omitempty"`  // 输出 token 上限；0 表示交由 provider 取默认值
	Temperature *float64         `json:"temperature,omitempty"` // 采样温度；用指针区分"未设置"与显式 0
	Schema      *json.RawMessage `json:"schema,omitempty"`      // 结构化输出的 JSON Schema，原样透传给支持该能力的 provider
	Effort      EffortLevel      `json:"effort,omitempty"`      // v1.4
	// Effort 推理档位随请求透传，由 provider 映射为自家推理强度参数（见 EffortLevel）。
}

// ChatResponse is the output of an LLM call.
// ChatResponse 是提供商无关的调用结果：文本、工具调用、用量与停止原因四要素。
type ChatResponse struct {
	Content    string     `json:"content"`              // 模型输出的文本内容
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"` // 模型请求的工具调用；非空时工具循环据此继续迭代
	Usage      Usage      `json:"usage"`                // 本次调用的 token/成本统计，供预算钩子累计
	StopReason string     `json:"stop_reason"`          // 停止原因（如自然结束/工具调用/长度截断），语义由 provider 归一化
}

// AsMessage converts a ChatResponse to a Message for conversation history.
// AsMessage 把模型响应转成 role="assistant" 的消息追加进历史，保持工具循环的上下文完整。
func (r *ChatResponse) AsMessage() Message {
	return Message{
		Role:      "assistant", // 模型侧发言固定为 assistant 角色
		Content:   r.Content,   // 输出文本原样入史
		ToolCalls: r.ToolCalls, // 工具调用请求也必须入史，后续 tool 消息才有配对对象
	}
}

// ToolDispatcher routes tool calls to their implementations.
// ToolDispatcher 是工具路由的统一抽象：列出可用工具、并把模型发起的调用派发到具体实现。
// 权限控制、MCP 接入等都以装饰器方式包裹这一接口实现，而无需内核感知。
type ToolDispatcher interface {
	ListTools(ctx context.Context) ([]ToolDef, error)                 // 枚举当前对模型可见的工具定义
	Dispatch(ctx context.Context, call ToolCall) (*ToolResult, error) // 执行一次工具调用并返回结果
}

// ToolDef describes a tool available to the LLM.
// ToolDef 是提供商无关的工具定义，由 provider 适配层转成各家 API 的工具声明格式。
type ToolDef struct {
	Name        string          `json:"name"`                // 工具唯一名，模型以此发起调用
	Description string          `json:"description"`         // 面向模型的用途说明，直接影响模型选用工具的准确度
	InputSchema json.RawMessage `json:"input_schema"`        // 入参的 JSON Schema，原样透传、contract 层不校验
	ReadOnly    bool            `json:"read_only,omitempty"` // v1.4: parallel dispatch hint
	// ReadOnly 是"只读、无副作用"声明：工具循环据此把同一轮内的多个只读调用安全地并行派发；
	// 有副作用的工具绝不能标记为只读，否则并行执行会破坏顺序语义。
}

// ToolHook intercepts individual tool calls within a ToolLoop.
// ToolHook 是工具循环内针对单次工具调用的拦截器：Matcher 选目标，Pre 前置改写/拦截，Post 后置审计。
type ToolHook struct {
	// Matcher selects which tools this hook applies to. nil = all tools.
	// Matcher 按工具名决定钩子是否生效；置 nil 表示对所有工具生效。
	Matcher func(toolName string) bool

	// Pre runs before dispatch. Can modify the call or return error to block.
	// Pre 在派发前执行：可返回改写后的 ToolCall（如参数脱敏、注入默认值），
	// 返回 error 则直接拦截本次调用，工具不会被执行。
	Pre func(ctx context.Context, call ToolCall) (ToolCall, error)

	// Post runs after dispatch. Can inspect/log result or return error to abort.
	// Post 在派发后执行：用于审计/记录结果；返回 error 会中止整个工具循环，
	// 但不能修改结果本身（result 仅供检查）。
	Post func(ctx context.Context, call ToolCall, result *ToolResult) error
}

// StreamSink receives streaming output from steps.
// StreamSink 是宿主接收步骤流式输出的界面：CLI、WebSocket、SSE 等各自实现，
// 步骤只管往 sink 推送，完全不关心输出最终去向。
type StreamSink interface {
	SendChunk(chunk StreamChunk) error          // 推送一个流式增量块（文本 delta / 终止块）
	SendEvent(eventType string, data any) error // 推送带类型的自定义事件（如工具开始/结束），供宿主渲染进度
	Close() error                               // 关闭 sink，通知宿主不会再有输出
}
