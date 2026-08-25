// Package openai implements contract.LLM for any OpenAI-compatible API.
// Works with OpenAI, DeepSeek, OpenRouter, Together, Ollama, vLLM, Azure OpenAI, etc.
//
// 中文说明：本包是 contract.LLM 接口的"OpenAI 兼容"供应商实现。
// 大量厂商（DeepSeek、OpenRouter、Together、Ollama、vLLM、Azure OpenAI 等）
// 都复用了 OpenAI 的 /chat/completions wire 协议，因此本实现只面向该协议本身，
// 通过可配置的基址（baseURL）与路径（chatPath）即可指向任意兼容端点。
// 内核与 stdlib 只依赖 contract 接口，不感知这里的任何 wire 细节。
package openai

import (
	"bufio"         // 按行扫描 SSE 流式响应体（bufio.Scanner）
	"bytes"         // 把序列化后的 JSON 请求体包装成 io.Reader
	"context"       // 透传调用方 context，承载取消与超时
	"encoding/json" // contract 类型与 OpenAI wire 格式之间的 JSON 编解码
	"errors"        // 哨兵错误：调用方用 errors.Is 做机器可判分类
	"fmt"           // 构造带 "openai:" 前缀的错误信息
	"io"            // 一次性读取非流式响应体 / 流式建连失败时的错误体
	"net/http"      // 底层 HTTP 客户端与请求构造
	"strings"       // SSE "data: " 行前缀解析、baseURL 尾部斜杠裁剪
	"time"          // 物理 attempt 超时（http.Client.Timeout）

	"github.com/jinyitao123/loom/contract" // 内核契约层：LLM 接口与请求/响应类型
)

// DefaultAttemptTimeout bounds one physical HTTP attempt when no explicit
// timeout is configured. It is deliberately finite: a single model generation
// should never hang the request forever (spec §4.4 — no unbounded waits), and
// 120s is well beyond a normal generation while still failing fast. 它只约束
// 单次物理 attempt；多轮 ToolLoop 的每一轮各占一次 attempt，互不累计。
const DefaultAttemptTimeout = 120 * time.Second

// Sentinel errors for machine-readable classification (errors.Is).
// 哨兵错误只承载协议分类，绝不携带 prompt/响应正文或凭据（spec §4.1 保密边界）。
var (
	// ErrEmptyResponse 中文：HTTP 200 但 content 与 tool_calls 均为空
	//（非流式），或流式 [DONE] 到达但全程无 content 且无 tool_calls。
	// 属于可重试错误：模型侧空响应，先重试当前模型，耗尽后走 fallback 链。
	ErrEmptyResponse = errors.New("openai: empty response")
	// ErrStreamClosedWithoutDone 中文：流在未见 [DONE] 的情况下 EOF 收尾。
	ErrStreamClosedWithoutDone = errors.New("openai: stream closed without [DONE]")
	// ErrStreamScanner 中文：SSE 扫描器报错（如单行超过 buffer 上限）。
	ErrStreamScanner = errors.New("openai: stream scanner error")
	// ErrStreamCancelled 中文：流读取期间调用方 context 被取消/超时。
	ErrStreamCancelled = errors.New("openai: stream context cancelled")
	// ErrJSONModeInstructionMissing 中文：json_object 模式 + Schema 请求在发送前
	// 校验消息中缺少明确的 JSON 输出指令。属调用方配置错误，重试无益（非可重试）。
	ErrJSONModeInstructionMissing = errors.New("openai: json mode requires an explicit JSON instruction in messages")
)

// EmptyResponseError carries protocol context for an empty model response.
// 上下文字段（finish_reason、provider 收尾形态）是 M3 报告与后续观测的数据源；
// 不记正文。
type EmptyResponseError struct {
	FinishReason string // provider finish_reason（stop / length / tool_calls ...）
	Stream       bool   // true 表示在流式路径上检出
	CloseKind    string // 流式收尾形态："done" / "eof" / "scanner" / "cancel"；非流式留空
}

func (e *EmptyResponseError) Error() string {
	if e.FinishReason != "" {
		return "openai: empty response (finish_reason=" + e.FinishReason + ")"
	}
	return "openai: empty response"
}

// Unwrap 让 errors.Is(err, ErrEmptyResponse) 成立。
func (e *EmptyResponseError) Unwrap() error { return ErrEmptyResponse }

// StreamCloseError describes an abnormal stream termination that must reach
// the consumer as an error rather than a clean close.
type StreamCloseError struct {
	CloseKind string // "eof" / "scanner" / "cancel"
	Err       error  // 底层错误（scanner 错误、ctx 错误）；可为 nil
}

func (e *StreamCloseError) Error() string {
	switch e.CloseKind {
	case "scanner":
		if e.Err != nil {
			return "openai: stream scanner error: " + e.Err.Error()
		}
		return "openai: stream scanner error"
	case "cancel":
		if e.Err != nil {
			return "openai: stream context cancelled: " + e.Err.Error()
		}
		return "openai: stream context cancelled"
	default:
		return "openai: stream closed without [DONE]"
	}
}

// Unwrap 让 errors.Is 精确命中对应的哨兵分类。
func (e *StreamCloseError) Unwrap() error {
	switch e.CloseKind {
	case "scanner":
		return ErrStreamScanner
	case "cancel":
		return ErrStreamCancelled
	default:
		return ErrStreamClosedWithoutDone
	}
}

// Client implements contract.LLM for any OpenAI-compatible endpoint.
// 中文：面向任意 OpenAI 兼容端点的 LLM 客户端。字段均为私有、构造完成后只读，
// 因此可被多个 goroutine 并发复用；配置只能经 New + Option 注入。
type Client struct {
	// 中文：API 密钥，每次请求以 "Bearer <key>" 写入 Authorization 头。
	apiKey string
	// 中文：API 基址（默认 https://api.openai.com）；替换它即可把同一实现
	// 指向 DeepSeek、OpenRouter、Ollama、vLLM、Azure OpenAI 等任何兼容端点。
	baseURL string
	// 中文：拼接到 baseURL 之后的 chat completions 路径（默认 /v1/chat/completions）。
	chatPath string // path appended to baseURL for chat completions
	// 中文：请求未显式指定模型时使用的兜底模型名。
	defaultModel string
	// 中文：底层 HTTP 客户端。默认带 120s 的物理 attempt 超时
	//（WithAttemptTimeout 可覆盖），与调用方传入的 context 取消/超时叠加生效。
	client *http.Client
	// 中文：为 true 时结构化输出退化为 json_object（不下发 schema），
	// 供不支持 OpenAI json_schema 模式的厂商（如 DeepSeek）使用。
	jsonObjectMode bool // use json_object instead of json_schema for Schema requests
	// 中文：DeepSeek 方言 thinking 开关。非空（"enabled"/"disabled"）表示 provider
	// 已显式 opt-in，请求体会携带 thinking 键；空串表示未 opt-in，thinking 键
	// 绝不出现（OpenAI/OneAPI 等兼容端点请求体零回归）。
	thinkingDefaultMode string
	// 中文：携带 tools 时强制 thinking=disabled。DeepSeek 官方 OMP 适配要求
	// thinking+tools 必须回传 reasoning_content；M1 阶段不做 reasoning 连续性，
	// 因此带 tools 的请求直接 fail-safe 关闭 thinking。
	thinkingDisableWithTools bool
}

// Option configures a Client.
// 中文：函数式选项（functional options）模式，在 New 构造期定制 Client。
type Option func(*Client)

// WithBaseURL sets the API base URL (default: https://api.openai.com).
// 中文：覆盖 API 基址，用于把客户端指向任意 OpenAI 兼容端点。
func WithBaseURL(url string) Option {
	// 裁掉尾部 "/"：避免与 chatPath 拼接时产生 "//" 导致部分服务端 404。
	return func(c *Client) { c.baseURL = strings.TrimRight(url, "/") }
}

// WithDefaultModel sets the default model when none is specified in the request.
// 中文：设置兜底模型名；仅当 ChatRequest.Model 为空时生效。
func WithDefaultModel(model string) Option {
	return func(c *Client) { c.defaultModel = model } // 直接覆盖默认值
}

// WithJSONObjectMode uses the simpler json_object response_format instead of
// json_schema for structured output. Required for providers (e.g. DeepSeek)
// that don't support OpenAI's json_schema mode.
// 中文：把结构化输出从 json_schema 降级为 json_object。降级后不下发 schema，
// 只约束"输出必须是 JSON 对象"，schema 约束需靠 prompt 自行保证；
// DeepSeek 等不支持 json_schema 的厂商必须开启此选项。
func WithJSONObjectMode() Option {
	return func(c *Client) { c.jsonObjectMode = true } // 打开兼容开关
}

// WithThinkingControl opts the client into the DeepSeek-thinking wire field.
// defaultMode is "enabled" or "disabled" and applies to requests without
// tools; when disableWithTools is true, requests carrying tools force
// thinking.type=disabled (DeepSeek's OMP-compatible guidance). Passing an
// empty defaultMode keeps the client on the plain OpenAI-compatible wire:
// the thinking key is never emitted, so OpenAI/OneAPI endpoints see zero
// request changes.
// 中文：显式开启 DeepSeek thinking 能力。defaultMode 为无 tools 请求的
// thinking 类型（"enabled"/"disabled"）；disableWithTools=true 时携带 tools
// 的请求强制 thinking=disabled。defaultMode 传空串表示未 opt-in——thinking
// 键绝不发送，OpenAI/OneAPI 等端点的请求体与历史完全一致（防回归）。
func WithThinkingControl(defaultMode string, disableWithTools bool) Option {
	return func(c *Client) {
		c.thinkingDefaultMode = defaultMode
		c.thinkingDisableWithTools = disableWithTools
	}
}

// WithChatPath overrides the path appended to the base URL for chat completions
// (default: /v1/chat/completions). Needed by OpenAI-compatible providers whose
// base URL already carries a version segment, e.g. Zhipu GLM, whose endpoint is
// https://open.bigmodel.cn/api/paas/v4/chat/completions.
// 中文：覆盖 chat completions 路径。适用于基址本身已带版本段的厂商
// （如智谱 GLM），此时默认的 /v1/chat/completions 会拼出错误 URL。
func WithChatPath(path string) Option {
	return func(c *Client) { c.chatPath = path } // 直接覆盖默认路径
}

// WithAttemptTimeout bounds one physical HTTP attempt via http.Client.Timeout
// (headers + body). d <= 0 keeps the package default (DefaultAttemptTimeout).
// 中文：约束单个物理 attempt 的总时长（含读流式 body）。作用于
// http.Client.Timeout，与调用方 context 超时是叠加关系——两者先到先生效。
// d<=0 时保持包默认值 120s。
func WithAttemptTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.client.Timeout = d
		}
	}
}

// New creates a new OpenAI-compatible client.
// 中文：构造客户端。先填入指向 OpenAI 官方端点的整套默认值，
// 再按传入顺序应用 Option 覆盖（后写的选项生效）。
func New(apiKey string, opts ...Option) *Client {
	// 默认配置：官方基址 + 标准路径 + gpt-4o 兜底模型 + 进程级共享的 HTTP 客户端。
	c := &Client{
		apiKey:       apiKey,                   // 保存密钥，供每次请求组装 Authorization 头
		baseURL:      "https://api.openai.com", // 默认基址；WithBaseURL 可换成任意兼容端点
		chatPath:     "/v1/chat/completions",   // 默认 chat completions 路径；WithChatPath 可覆盖
		defaultModel: "gpt-4o",                 // 请求未带模型名时的兜底值；WithDefaultModel 可覆盖
		// 每个客户端持有一个独立的 *http.Client（共享 http.DefaultTransport 连接池），
		// 这样 WithAttemptTimeout 只会改动本客户端，绝不污染进程级 http.DefaultClient。
		client: &http.Client{Timeout: DefaultAttemptTimeout},
	}
	// 依次应用调用方给的选项，覆盖上面的默认值。
	for _, opt := range opts {
		opt(c)
	}
	return c // 返回配置完成、此后只读的客户端
}

// openAI-compatible request/response types.
// 中文：以下为 OpenAI /chat/completions 协议的 wire 类型，字段与官方 JSON 一一对应；
// 仅在本包内部使用，收发两端各做一次与 contract 类型的映射。

type oaiRequest struct {
	Model          string             `json:"model"`                     // 模型名（已经过 resolveModel 兜底）
	Messages       []oaiMessage       `json:"messages"`                  // 对话历史，由 contract.Message 映射而来
	Tools          []oaiTool          `json:"tools,omitempty"`           // 工具定义列表；为 nil 时整个字段缺省
	MaxTokens      int                `json:"max_tokens,omitempty"`      // 输出 token 上限；0 时省略，交给服务端默认
	Temperature    *float64           `json:"temperature,omitempty"`     // 采样温度；用指针区分"未设置"与显式 0
	Stream         bool               `json:"stream,omitempty"`          // true 时服务端切换为 SSE 流式返回
	StreamOptions  *oaiStreamOptions  `json:"stream_options,omitempty"`  // 流式附加选项（仅流式请求携带）
	ResponseFormat *oaiResponseFormat `json:"response_format,omitempty"` // 结构化输出模式（json_object / json_schema）
	// ReasoningEffort maps contract.ChatRequest.Effort for reasoning models
	// (gpt-5.x / o-series). Omitted when unset; models that don't support the
	// parameter reject it, so callers opt in per request.
	// 中文：contract.ChatRequest.Effort 的透传落点，序列化为 reasoning_effort；
	// 未设置时靠 omitempty 整体省略——不支持该参数的模型会直接拒绝请求，
	// 所以是否携带由调用方按请求粒度自行决定。
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// Thinking 中文：DeepSeek 方言的 thinking 开关（{"type":"enabled"|"disabled"}）。
	// 仅当构造时经 WithThinkingControl 显式 opt-in 才携带；未 opt-in 时整个字段
	// 缺省，保证 OpenAI/OneAPI 等兼容端点的请求体与历史完全一致。
	Thinking *oaiThinking `json:"thinking,omitempty"`
}

// oaiThinking 中文：DeepSeek thinking 开关的 wire 载体；Type 仅允许
// "enabled" / "disabled"（DeepSeek 方言字段，OpenAI 官方协议没有该键）。
type oaiThinking struct {
	Type string `json:"type"`
}

// oaiResponseFormat 中文：response_format 字段的载体。json_object 模式只填 Type；
// json_schema 模式还要附带完整的 schema 描述。
type oaiResponseFormat struct {
	Type       string         `json:"type"`                  // "json_object" 或 "json_schema"
	JSONSchema *oaiJSONSchema `json:"json_schema,omitempty"` // 仅 json_schema 模式下发
}

// oaiJSONSchema 中文：json_schema 模式携带的 schema 描述体。
type oaiJSONSchema struct {
	Name   string          `json:"name"`   // schema 名称，协议必填（本包固定用 "output"）
	Strict bool            `json:"strict"` // true 时服务端严格校验输出必须完全匹配 schema
	Schema json.RawMessage `json:"schema"` // 调用方 JSON Schema 原文，RawMessage 原样透传不解析
}

// oaiStreamOptions 中文：流式请求的附加选项。
type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"` // true 时服务端在流末尾补发一个带 usage 统计的专用增量块
}

// oaiMessage 中文：wire 格式的单条消息，四类角色共用同一结构：
// system/user 只用 Role+Content；assistant 可能携带 ToolCalls（历史回放）；
// tool 角色用 ToolCallID 标明自己是哪一次调用的执行结果。
type oaiMessage struct {
	Role       string        `json:"role"`                   // 消息角色：system / user / assistant / tool
	Content    string        `json:"content,omitempty"`      // 文本内容；纯工具调用的 assistant 消息可为空
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`   // assistant 消息回放的历史工具调用
	ToolCallID string        `json:"tool_call_id,omitempty"` // tool 消息与先前某次调用配对的 ID
}

// oaiTool 中文：请求中的工具定义外壳，type 固定为 "function"。
type oaiTool struct {
	Type     string          `json:"type"`     // 协议固定值 "function"
	Function oaiToolFunction `json:"function"` // 具体的函数签名描述
}

// oaiToolFunction 中文：function 工具的签名，由 contract.ToolDef 映射而来。
type oaiToolFunction struct {
	Name        string          `json:"name"`        // 工具名，模型据此发起调用
	Description string          `json:"description"` // 工具用途描述，供模型选择工具时参考
	Parameters  json.RawMessage `json:"parameters"`  // 参数 JSON Schema 原文（即 ToolDef.InputSchema）
}

// oaiToolCall 中文：一次工具调用的 wire 表示；请求侧（历史回放）与
// 响应侧（含流式增量块）复用同一结构。
type oaiToolCall struct {
	Index int    `json:"index"` // 流式场景下的调用序号，用于跨增量块归并同一次调用
	ID    string `json:"id"`    // 供应商生成的调用 ID，回传工具结果时据此配对
	Type  string `json:"type"`  // 固定为 "function"
	// Function 中文：调用目标与参数；Arguments 是 JSON 字符串，
	// 流式时按片段递增送达，需要在客户端拼接还原。
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// oaiResponse 中文：非流式响应体。本包只消费首个 choice；注意错误既可能表现为
// 非 200 状态码，也可能藏在 200 响应体的 error 字段里（部分兼容端点的行为），
// 因此两条路径都要检查。
type oaiResponse struct {
	Choices []struct {
		// Message 中文：模型给出的完整回复（文本 + 可选的工具调用列表）。
		Message struct {
			Role      string        `json:"role"`                 // 恒为 assistant
			Content   string        `json:"content"`              // 回复文本
			ToolCalls []oaiToolCall `json:"tool_calls,omitempty"` // 模型请求执行的工具调用
		} `json:"message"`
		FinishReason string `json:"finish_reason"` // 结束原因：stop / tool_calls / length 等
	} `json:"choices"`
	// Usage 中文：本次调用的 token 统计，映射回 contract.Usage。
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`     // 输入侧 token 数
		CompletionTokens int `json:"completion_tokens"` // 输出侧 token 数
	} `json:"usage"`
	// Error 中文：响应体内嵌的 API 错误（可能伴随 200 状态码出现）。
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// resolveModel 中文：模型名解析——请求显式指定则优先，空串时回退到构造期的 defaultModel。
func (c *Client) resolveModel(model string) string {
	// 显式指定的模型优先生效。
	if model != "" {
		return model
	}
	return c.defaultModel // 未指定时使用兜底模型
}

// buildMessages 中文：把 contract.Message 列表逐条映射为 OpenAI wire 消息。
// 角色字符串（system/user/assistant/tool）两侧约定一致，直接透传；
// assistant 消息里的历史 ToolCalls 必须"回放"给服务端，tool 角色消息则靠
// ToolCallID 与先前的调用配对——这两者是多轮工具调用能够续接的关键。
func (c *Client) buildMessages(msgs []contract.Message) []oaiMessage {
	out := make([]oaiMessage, len(msgs)) // 一次分配，与输入一一对应
	for i, m := range msgs {
		// 基础字段映射：角色、文本内容、工具结果消息配对用的 tool_call_id。
		out[i] = oaiMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		// 回放该条消息（通常是 assistant）曾发起的工具调用：
		// wire 格式要求 type 固定为 "function"，参数保持原始 JSON 字符串不解析。
		for _, tc := range m.ToolCalls {
			out[i].ToolCalls = append(out[i].ToolCalls, oaiToolCall{
				ID:   tc.ID,      // 沿用供应商当初返回的调用 ID，服务端靠它与 tool 消息配对
				Type: "function", // OpenAI 协议目前唯一的工具调用类型
				// 匿名结构体字面量需复述 JSON tag，与 oaiToolCall.Function 的定义保持一致。
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: tc.Name, Arguments: tc.Args}, // 工具名 + 原始参数 JSON 原样透传
			})
		}
	}
	return out
}

// buildTools 中文：把 contract.ToolDef 映射为 OpenAI 的 function 工具定义。
// InputSchema 是 json.RawMessage，直接作为 parameters 下发，不做校验或改写；
// 入参为空时返回 nil，配合 oaiRequest.Tools 的 omitempty 让 "tools" 字段整体缺省。
func (c *Client) buildTools(tools []contract.ToolDef) []oaiTool {
	var out []oaiTool // 保持 nil 语义：无工具时不出现在请求 JSON 中
	for _, t := range tools {
		out = append(out, oaiTool{
			Type: "function", // 协议固定值
			Function: oaiToolFunction{
				Name:        t.Name,        // 工具名
				Description: t.Description, // 描述文本，供模型理解工具用途
				Parameters:  t.InputSchema, // 参数 JSON Schema 原样透传
			},
		})
	}
	return out
}

// applyThinkingControl 中文：按 provider 显式声明的 capability 填充 thinking 键，
// 并裁剪 thinking 模式不支持的参数（temperature）。未 opt-in（defaultMode 为空）
// 时零改动——thinking 键绝不出现、temperature 照常透传。
// disableWithTools=true 且请求携带 tools 时强制 disabled（M1 fail-safe：不做
// reasoning 连续性回传，直接关闭 thinking 以避免 DeepSeek 400/空响应）。
func (c *Client) applyThinkingControl(oaiReq *oaiRequest) {
	if c.thinkingDefaultMode == "" {
		return // 未 opt-in：OpenAI/OneAPI 等端点请求体零变化
	}
	thinkingType := c.thinkingDefaultMode
	if c.thinkingDisableWithTools && len(oaiReq.Tools) > 0 {
		thinkingType = "disabled"
	}
	switch thinkingType {
	case "enabled", "disabled":
		oaiReq.Thinking = &oaiThinking{Type: thinkingType}
	default:
		// 未知值不落 wire：thinking 键绝不出现，temperature 保持现状（fail-safe）。
		return
	}
	if thinkingType == "enabled" {
		// 官方文档：thinking 模式不支持 temperature/top_p/presence/frequency
		// penalty；裁剪 temperature，避免平台显示一个实际不生效的配置。
		oaiReq.Temperature = nil
	}
}

// requireJSONOutputInstruction is the pre-send guard for json_object mode.
// Providers like DeepSeek only guarantee "the output is a JSON object" when
// the prompt explicitly asks for JSON; without the instruction the model may
// stream blanks until the token cap (official docs), so the request must fail
// before leaving the client. The match is case-insensitive "json", which also
// covers Chinese prompts that embed the ASCII term (e.g. "请输出JSON格式").
// 中文：json_object 模式发送前校验 messages 中存在明确的 JSON 输出指令；
// 缺失时返回 ErrJSONModeInstructionMissing，请求不得发出（server 零命中）。
func (c *Client) requireJSONOutputInstruction(req contract.ChatRequest) error {
	if !c.jsonObjectMode || req.Schema == nil {
		return nil
	}
	for _, m := range req.Messages {
		if strings.Contains(strings.ToLower(m.Content), "json") {
			return nil
		}
	}
	return fmt.Errorf("%w", ErrJSONModeInstructionMissing)
}

// Chat implements contract.LLM.
// 中文：非流式对话。把 contract.ChatRequest 映射为 OpenAI wire 请求，
// 同步 POST 一次，再把首个 choice 回填为 contract.ChatResponse。
// 本方法不做重试；单个物理 attempt 受构造期超时约束（默认 120s，WithAttemptTimeout
// 可覆盖），与调用方传入的 ctx 取消/超时叠加生效。
func (c *Client) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	// json_object + Schema 的发送前校验：缺 JSON 指令立即失败，请求不出门。
	if err := c.requireJSONOutputInstruction(req); err != nil {
		return nil, err
	}
	// 组装 wire 请求：模型名兜底、消息/工具逐一映射；
	// Effort 直接透传为 reasoning_effort（空值因 omitempty 不会出现在 JSON 中）。
	oaiReq := oaiRequest{
		Model:           c.resolveModel(req.Model),     // 未指定模型时回退到 defaultModel
		Messages:        c.buildMessages(req.Messages), // contract.Message -> oaiMessage（含 tool_calls 回放）
		Tools:           c.buildTools(req.Tools),       // contract.ToolDef -> OpenAI function 工具定义
		MaxTokens:       req.MaxTokens,                 // 输出 token 上限；0 表示交给服务端默认
		Temperature:     req.Temperature,               // 指针：nil 表示"用服务端默认"，可与显式 0 区分
		ReasoningEffort: string(req.Effort),            // ChatRequest.Effort 透传为 reasoning_effort（推理模型专用）
	}
	// thinking capability：provider 显式 opt-in 才携带 thinking 键；
	// thinking enabled 时裁剪 temperature（见 applyThinkingControl）。
	c.applyThinkingControl(&oaiReq)
	// 结构化输出：调用方提供了 JSON Schema 时设置 response_format。
	if req.Schema != nil {
		if c.jsonObjectMode {
			// 兼容模式：只声明"输出必须是 JSON 对象"，不下发具体 schema
			//（DeepSeek 等不支持 json_schema 的厂商走这里，schema 约束靠 prompt 保证）。
			oaiReq.ResponseFormat = &oaiResponseFormat{Type: "json_object"}
		} else {
			// 严格模式：下发完整 json_schema 并开启 strict，由服务端强制输出符合 schema。
			oaiReq.ResponseFormat = &oaiResponseFormat{
				Type: "json_schema",
				JSONSchema: &oaiJSONSchema{
					Name:   "output",    // schema 名称为协议必填，取固定值即可
					Strict: true,        // 严格校验：输出必须完全匹配 schema
					Schema: *req.Schema, // 调用方的 JSON Schema 原样透传（RawMessage 不做二次解析）
				},
			}
		}
	}

	// 序列化请求体；失败通常意味着调用方传入了非法 schema 等编程错误。
	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}

	// 构造 HTTP 请求并绑定 ctx：取消/超时由调用方 context 驱动，本包不额外设限。
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+c.chatPath, bytes.NewReader(body))
	if err != nil {
		return nil, err // URL 非法等构造错误，原样返回
	}
	httpReq.Header.Set("Content-Type", "application/json")  // 请求体为 JSON
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey) // OpenAI 风格的 Bearer 鉴权

	// 发起同步请求；网络层错误（连接失败、ctx 取消等）在此返回。
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: http: %w", err)
	}
	defer resp.Body.Close() // 无论成败都释放连接

	// 先整体读出响应体：非 200 时它就是错误详情，200 时再做 JSON 解析。
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai: read response: %w", err)
	}

	// 非 200 一律视为失败，把原始响应体附进错误便于排查（通常含厂商错误 JSON）。
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// 解析 wire 响应。
	var oaiResp oaiResponse
	if err := json.Unmarshal(respBody, &oaiResp); err != nil {
		return nil, fmt.Errorf("openai: unmarshal response: %w", err)
	}

	// 部分兼容端点会在 200 响应体内携带 error 字段，需要单独识别。
	if oaiResp.Error != nil {
		return nil, fmt.Errorf("openai: API error: %s", oaiResp.Error.Message)
	}

	// 协议约定至少返回一个 choice；空响应视为异常。
	if len(oaiResp.Choices) == 0 {
		return nil, fmt.Errorf("openai: empty response")
	}

	// 只取第一个 choice（框架不使用 n>1 的多候选），回填为 contract.ChatResponse。
	choice := oaiResp.Choices[0]
	chatResp := &contract.ChatResponse{
		Content:    choice.Message.Content, // 助手文本内容
		StopReason: choice.FinishReason,    // 结束原因（stop / tool_calls / length ...）原样透传
		Usage: contract.Usage{
			InputTokens:  oaiResp.Usage.PromptTokens,     // prompt_tokens -> 输入 token 数
			OutputTokens: oaiResp.Usage.CompletionTokens, // completion_tokens -> 输出 token 数
		},
	}

	// 把模型发起的 tool_calls 逐个映射回 contract.ToolCall；
	// Args 保持为原始 JSON 字符串，由上层（引擎/工具执行器）负责解析。
	for _, tc := range choice.Message.ToolCalls {
		chatResp.ToolCalls = append(chatResp.ToolCalls, contract.ToolCall{
			ID:   tc.ID,                 // 调用 ID，后续回传工具结果时用于配对
			Name: tc.Function.Name,      // 工具名
			Args: tc.Function.Arguments, // 参数 JSON 字符串（原样保留）
		})
	}

	// 空响应 fail-closed：HTTP 200 但 content 与 tool_calls 均为空不是成功。
	// 合法例外：tool_calls 非空时 content 可以为空字符串。
	if chatResp.Content == "" && len(chatResp.ToolCalls) == 0 {
		return nil, &EmptyResponseError{FinishReason: choice.FinishReason}
	}

	return chatResp, nil // 映射完成，返回 contract 层响应
}

// oaiStreamChunk is one SSE chunk from the streaming API.
// 中文：SSE 流式响应中的单个增量块（一条 "data:" 行反序列化的结果）。
type oaiStreamChunk struct {
	Choices []struct {
		// Delta 中文：本增量块相对于之前的"新增部分"——文本增量或 tool_calls 参数增量。
		Delta struct {
			Content   string        `json:"content,omitempty"`    // 本块新增的文本片段
			ToolCalls []oaiToolCall `json:"tool_calls,omitempty"` // 本块新增的工具调用增量（按 Index 归并）
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"` // 指针区分"尚未结束"（null）与具体结束原因
	} `json:"choices"`
	// Usage 中文：仅在开启 stream_options.include_usage 后，于流末尾一个
	// choices 为空的专用增量块中出现；其余块均为 nil。
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`     // 输入侧 token 数
		CompletionTokens int `json:"completion_tokens"` // 输出侧 token 数
	} `json:"usage,omitempty"`
}

// Stream implements contract.LLM with SSE streaming.
// 中文：流式对话。请求组装与 Chat 完全一致，只额外打开 stream 开关并请求 usage 统计；
// 立即返回一个由后台 goroutine 持续写入的只读通道：文本增量即时下发，
// tool_calls 参数按 Index 跨块拼接，直到 [DONE] 哨兵到达时随终止块一次性交付。
func (c *Client) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	// json_object + Schema 的发送前校验：与 Chat 同一语义，请求不得发出。
	if err := c.requireJSONOutputInstruction(req); err != nil {
		return nil, err
	}
	// 组装 wire 请求：映射逻辑与 Chat 相同（含 Effort -> reasoning_effort 透传），
	// 差异仅两处——Stream=true 切换 SSE；StreamOptions 让服务端在流尾补发 usage。
	oaiReq := oaiRequest{
		Model:           c.resolveModel(req.Model),             // 模型名兜底
		Messages:        c.buildMessages(req.Messages),         // 消息映射（含 tool_calls 回放）
		Tools:           c.buildTools(req.Tools),               // 工具定义映射
		MaxTokens:       req.MaxTokens,                         // 输出上限
		Temperature:     req.Temperature,                       // nil 表示服务端默认
		ReasoningEffort: string(req.Effort),                    // Effort 透传（omitempty，未设置不发送）
		Stream:          true,                                  // 开启 SSE 流式返回
		StreamOptions:   &oaiStreamOptions{IncludeUsage: true}, // 请求服务端在流末尾附带 usage 统计块
	}
	// thinking capability：与 Chat 完全一致的 opt-in 语义与 temperature 裁剪。
	c.applyThinkingControl(&oaiReq)
	// 结构化输出设置：与 Chat 相同的两种模式。
	if req.Schema != nil {
		if c.jsonObjectMode {
			// 兼容模式：仅约束"输出为 JSON 对象"。
			oaiReq.ResponseFormat = &oaiResponseFormat{Type: "json_object"}
		} else {
			// 严格模式：下发完整 json_schema 并开启 strict。
			oaiReq.ResponseFormat = &oaiResponseFormat{
				Type: "json_schema",
				JSONSchema: &oaiJSONSchema{
					Name:   "output",    // 固定 schema 名
					Strict: true,        // 严格校验
					Schema: *req.Schema, // 调用方 schema 原样透传
				},
			}
		}
	}

	// 序列化请求体。
	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}

	// 构造请求并绑定 ctx：取消 ctx 会中断后续的流读取，使读循环自然退出。
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+c.chatPath, bytes.NewReader(body))
	if err != nil {
		return nil, err // URL 构造失败
	}
	httpReq.Header.Set("Content-Type", "application/json")  // 请求体仍是 JSON
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey) // Bearer 鉴权
	httpReq.Header.Set("Accept", "text/event-stream")       // 声明期望 SSE 流式响应

	// 发起请求；成功后响应体是一条保持打开的 SSE 长连接。
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: http: %w", err)
	}

	// 非 200 说明流根本没有建立：读出错误体、关闭连接，同步返回错误。
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(resp.Body) // 错误详情尽力而为，读失败也不影响主错误
		return nil, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, string(errBody))
	}

	// 带缓冲的增量块通道：允许解析端小幅领先消费端，降低相互阻塞。
	ch := make(chan contract.StreamChunk, 32)

	// 后台 goroutine 承担整个 SSE 解析生命周期；本函数随即返回通道。
	go func() {
		defer resp.Body.Close() // 退出时释放连接
		defer close(ch)         // 关闭通道即向消费方宣告流结束（含异常提前结束）

		// tool_calls 的参数 JSON 被拆散在多个增量块里，
		// 以 choice 内的 Index 为键在此累积拼接，直到流结束才完整。
		toolCallMap := make(map[int]*contract.ToolCall)
		var lastUsage *contract.Usage // 暂存流尾 usage 块，随最终 Done 块一并交付
		var lastFinishReason string   // 暂存流中首个非空 finish_reason，随终止块交付
		sawContent := false           // 是否交付过任何文本增量（空响应判定用）

		// 按行扫描 SSE 流：每个事件是一行 "data: {...}"，空行为事件分隔符。
		scanner := bufio.NewScanner(resp.Body)
		// 扩大 scanner buffer：初始 64KiB，单行上限 1MiB，杜绝超长行撑爆默认 64KiB
		// 后被静默截断；超限行会以 scanner.Err() 的形式被报告，而不是悄悄丢数据。
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text() // 当前行文本（不含换行符）

			// 只处理 "data:" 载荷行（冒号后允许零或多个空格）；空行、注释、
			// 其他 SSE 字段一律跳过。
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			// 剥掉前缀并去掉首尾空白，兼容 "data: {...}" 与 "data:{...}" 两种写法。
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			// "[DONE]" 是 OpenAI 协议的流结束哨兵（非 JSON），到达即收尾。
			if data == "[DONE]" {
				// 空响应 fail-closed：全程无文本且无工具调用，[DONE] 不是成功。
				if !sawContent && len(toolCallMap) == 0 {
					ch <- contract.StreamChunk{
						Err: &EmptyResponseError{
							FinishReason: lastFinishReason,
							Stream:       true,
							CloseKind:    "done",
						},
					}
					return
				}
				// 有累积的工具调用时，按 Index 0..n-1 依序还原为切片，
				// 与 Done 标志、usage 统计一起放进最后一个增量块交付。
				if len(toolCallMap) > 0 {
					var tcs []contract.ToolCall
					// 服务端按 0 起连续编号 Index，计数循环保证输出顺序稳定。
					for i := 0; i < len(toolCallMap); i++ {
						if tc, ok := toolCallMap[i]; ok {
							tcs = append(tcs, *tc)
						}
					}
					ch <- contract.StreamChunk{
						ToolCalls:    tcs,
						Done:         true,
						Usage:        lastUsage,
						FinishReason: lastFinishReason,
					}
				} else {
					// 纯文本流：只发终止块（Done + usage）。
					ch <- contract.StreamChunk{
						Done:         true,
						Usage:        lastUsage,
						FinishReason: lastFinishReason,
					}
				}
				return // 正常收尾：defer 负责关闭通道与连接
			}

			// 反序列化单个增量块；个别畸形块直接跳过，保证整条流的健壮性。
			var chunk oaiStreamChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}

			// Capture usage from any chunk (sent in a dedicated chunk
			// with empty choices when stream_options.include_usage=true).
			// 中文：usage 统计通常在流末尾一个 choices 为空的专用块中到达；
			// 这里对任意块做捕获并暂存，等 [DONE] 时随终止块下发。
			if chunk.Usage != nil {
				lastUsage = &contract.Usage{
					InputTokens:  chunk.Usage.PromptTokens,     // prompt_tokens -> 输入 token
					OutputTokens: chunk.Usage.CompletionTokens, // completion_tokens -> 输出 token
				}
			}

			// 捕获 finish_reason：服务端在最后一个内容块的 delta 上给出非 null 值，
			// 暂存后随终止块一并交付，供消费方区分 stop/length/tool_calls 等收尾。
			if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != nil &&
				*chunk.Choices[0].FinishReason != "" {
				lastFinishReason = *chunk.Choices[0].FinishReason
			}

			// choices 为空的块（如纯 usage 块）没有增量内容，跳过。
			if len(chunk.Choices) == 0 {
				continue
			}

			// 只消费第一个 choice 的增量（框架不使用多候选）。
			delta := chunk.Choices[0].Delta

			// 文本增量：即时透传给消费方，实现逐字输出。
			if delta.Content != "" {
				sawContent = true
				ch <- contract.StreamChunk{Content: delta.Content}
			}

			// 工具调用增量：首个分片携带 ID/Name 与参数开头，后续分片只带参数续片。
			for _, tc := range delta.ToolCalls {
				idx := tc.Index // 同一次调用的所有分片共享同一个 Index
				if _, ok := toolCallMap[idx]; !ok {
					// 首次见到该 Index：登记调用 ID、工具名与参数首片。
					toolCallMap[idx] = &contract.ToolCall{
						ID:   tc.ID,
						Name: tc.Function.Name,
						Args: tc.Function.Arguments,
					}
				} else {
					// 后续分片：把参数 JSON 续片追加到已登记的调用上。
					toolCallMap[idx].Args += tc.Function.Arguments
				}
			}
		}

		// 循环退出（未遇 [DONE]）：区分三种非正常收尾，必须让消费方收到错误
		// 而不是"干净关闭"。取消优先于 scanner 错误：ctx 取消时 body 读取失败
		// 的底层错误往往就是 context.Canceled/DeadlineExceeded。
		if ctx.Err() != nil {
			ch <- contract.StreamChunk{Err: &StreamCloseError{CloseKind: "cancel", Err: ctx.Err()}}
			return
		}
		if err := scanner.Err(); err != nil {
			ch <- contract.StreamChunk{Err: &StreamCloseError{CloseKind: "scanner", Err: err}}
			return
		}
		ch <- contract.StreamChunk{Err: &StreamCloseError{CloseKind: "eof"}}
	}()

	return ch, nil // 立即返回通道，解析在后台进行
}

// Compile-time interface check.
// 中文：编译期断言 *Client 实现了 contract.LLM；接口一旦变更此处立即报错。
var _ contract.LLM = (*Client)(nil)
