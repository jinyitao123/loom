// Package deepseek implements the contract.LLM interface for the DeepSeek API.
// DeepSeek uses an OpenAI-compatible API at https://api.deepseek.com.
//
// 中文说明：本包是 contract.LLM 的 DeepSeek 专用实现，走 DeepSeek 提供的
// OpenAI 兼容端点。与通用的 provider/openai 实现相比刻意做了收敛，差异点：
//   - 基址固定为 https://api.deepseek.com，不提供任何 Option 配置；
//   - 默认模型固定为 deepseek-chat；
//   - 结构化输出只支持 json_object（DeepSeek 不支持 OpenAI 的 json_schema 模式），
//     具体 schema 约束需要靠 prompt 自行保证；
//   - 不透传 ChatRequest.Effort（DeepSeek 通过选用 deepseek-reasoner 等模型
//     控制推理行为，而非 reasoning_effort 参数）。
package deepseek

import (
	"bufio"         // 按行扫描 SSE 流式响应体（bufio.Scanner）
	"bytes"         // 把序列化后的 JSON 请求体包装成 io.Reader
	"context"       // 透传调用方 context，承载取消与超时
	"encoding/json" // contract 类型与 DeepSeek（OpenAI 兼容）wire 格式的 JSON 编解码
	"fmt"           // 构造带 "deepseek:" 前缀的错误信息
	"io"            // 一次性读取非流式响应体 / 流式建连失败时的错误体
	"net/http"      // 底层 HTTP 客户端与请求构造
	"strings"       // SSE "data: " 行前缀解析

	"github.com/jinyitao123/loom/contract" // 内核契约层：LLM 接口与请求/响应类型
)

// Client implements contract.LLM for DeepSeek.
// 中文：DeepSeek 客户端。字段构造后只读，可被多 goroutine 并发使用；
// 与 provider/openai 不同，这里没有任何可配置项。
type Client struct {
	apiKey  string       // API 密钥，每次请求以 "Bearer <key>" 写入 Authorization 头
	baseURL string       // 固定为 https://api.deepseek.com，不开放覆盖
	client  *http.Client // 复用 http.DefaultClient；无固定超时、不做重试，全靠调用方 context 控制
}

// New creates a new DeepSeek client.
// 中文：构造 DeepSeek 客户端；只需 API 密钥，其余全部取固定默认值。
func New(apiKey string) *Client {
	return &Client{
		apiKey:  apiKey,                     // 保存密钥
		baseURL: "https://api.deepseek.com", // DeepSeek 官方端点（OpenAI 兼容协议）
		client:  http.DefaultClient,         // 共享全局 HTTP 客户端与连接池
	}
}

// openAI-compatible request/response types.
// 中文：以下为 OpenAI 兼容协议的 wire 类型（DeepSeek 沿用该协议），
// 字段与 JSON 一一对应；仅在本包内部使用，收发两端各做一次与 contract 类型的映射。

type oaiRequest struct {
	Model          string             `json:"model"`                     // 模型名（空时已兜底为 deepseek-chat）
	Messages       []oaiMessage       `json:"messages"`                  // 对话历史，由 contract.Message 映射而来
	Tools          []oaiTool          `json:"tools,omitempty"`           // 工具定义列表；为 nil 时整个字段缺省
	MaxTokens      int                `json:"max_tokens,omitempty"`      // 输出 token 上限；0 时省略，交给服务端默认
	Temperature    *float64           `json:"temperature,omitempty"`     // 采样温度；用指针区分"未设置"与显式 0
	Stream         bool               `json:"stream,omitempty"`          // true 时服务端切换为 SSE 流式返回
	StreamOptions  *oaiStreamOptions  `json:"stream_options,omitempty"`  // 流式附加选项（仅流式请求携带）
	ResponseFormat *oaiResponseFormat `json:"response_format,omitempty"` // 结构化输出模式（DeepSeek 仅支持 json_object）
	// 中文：注意与 provider/openai 的差异——这里没有 reasoning_effort 字段，
	// 即 ChatRequest.Effort 不会透传给 DeepSeek。
}

// oaiResponseFormat 中文：response_format 载体。DeepSeek 只支持 json_object，
// 因此不像 provider/openai 那样带 json_schema 子结构。
type oaiResponseFormat struct {
	Type string `json:"type"` // 固定填 "json_object"
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

// oaiResponse 中文：非流式响应体。本包只消费首个 choice；错误既可能表现为
// 非 200 状态码，也可能藏在 200 响应体的 error 字段里，两条路径都要检查。
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

// Chat implements contract.LLM.
// 中文：非流式对话。把 contract.ChatRequest 映射为 OpenAI 兼容 wire 请求，
// 同步 POST 一次，再把首个 choice 回填为 contract.ChatResponse。
// 本方法不做重试；超时与取消完全依赖调用方传入的 ctx。
// 注意：ChatRequest.Effort 在本实现中被忽略（wire 请求没有对应字段）。
func (c *Client) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	// Convert messages.
	// 中文：contract.Message -> oaiMessage 逐条映射。角色字符串两侧约定一致直接透传；
	// assistant 消息的历史 ToolCalls 需"回放"给服务端，tool 消息靠 ToolCallID 配对。
	msgs := make([]oaiMessage, len(req.Messages)) // 一次分配，与输入一一对应
	for i, m := range req.Messages {
		// 基础字段映射：角色、文本内容、工具结果消息配对用的 tool_call_id。
		msgs[i] = oaiMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		// 回放该条消息曾发起的工具调用：type 固定为 "function"，参数保持原始 JSON 不解析。
		for _, tc := range m.ToolCalls {
			msgs[i].ToolCalls = append(msgs[i].ToolCalls, oaiToolCall{
				ID:   tc.ID,      // 沿用供应商当初返回的调用 ID，服务端靠它与 tool 消息配对
				Type: "function", // 协议目前唯一的工具调用类型
				// 匿名结构体字面量需复述 JSON tag，与 oaiToolCall.Function 定义一致。
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: tc.Name, Arguments: tc.Args}, // 工具名 + 原始参数 JSON 原样透传
			})
		}
	}

	// Convert tools.
	// 中文：contract.ToolDef -> function 工具定义；InputSchema 原样下发。
	// tools 保持 nil 语义：无工具时配合 omitempty 使 "tools" 字段整体缺省。
	var tools []oaiTool
	for _, t := range req.Tools {
		tools = append(tools, oaiTool{
			Type: "function", // 协议固定值
			Function: oaiToolFunction{
				Name:        t.Name,        // 工具名
				Description: t.Description, // 描述文本，供模型理解工具用途
				Parameters:  t.InputSchema, // 参数 JSON Schema 原样透传
			},
		})
	}

	// 模型名兜底：未指定时使用 DeepSeek 的通用对话模型 deepseek-chat。
	model := req.Model
	if model == "" {
		model = "deepseek-chat"
	}

	// 组装 wire 请求（非流式：不设置 Stream / StreamOptions）。
	oaiReq := oaiRequest{
		Model:       model,           // 已兜底的模型名
		Messages:    msgs,            // 映射好的消息列表
		Tools:       tools,           // 映射好的工具定义
		MaxTokens:   req.MaxTokens,   // 输出 token 上限；0 表示交给服务端默认
		Temperature: req.Temperature, // 指针：nil 表示"用服务端默认"，可与显式 0 区分
	}
	// 结构化输出：DeepSeek 只支持 json_object 模式，仅约束"输出是 JSON 对象"；
	// 具体 schema 无法下发，需要靠 prompt 自行保证（与 provider/openai 的差异点）。
	if req.Schema != nil {
		oaiReq.ResponseFormat = &oaiResponseFormat{Type: "json_object"}
	}

	// 序列化请求体。
	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("deepseek: marshal request: %w", err)
	}

	// 构造 HTTP 请求并绑定 ctx：路径固定为 /v1/chat/completions，不可配置。
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err // URL 非法等构造错误，原样返回
	}
	httpReq.Header.Set("Content-Type", "application/json")  // 请求体为 JSON
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey) // Bearer 鉴权

	// 发起同步请求；网络层错误（连接失败、ctx 取消等）在此返回。
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("deepseek: http: %w", err)
	}
	defer resp.Body.Close() // 无论成败都释放连接

	// 先整体读出响应体：非 200 时它就是错误详情，200 时再做 JSON 解析。
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("deepseek: read response: %w", err)
	}

	// 非 200 一律视为失败，把原始响应体附进错误便于排查。
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("deepseek: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// 解析 wire 响应。
	var oaiResp oaiResponse
	if err := json.Unmarshal(respBody, &oaiResp); err != nil {
		return nil, fmt.Errorf("deepseek: unmarshal response: %w", err)
	}

	// 响应体内嵌的 error 字段可能伴随 200 状态码出现，需单独识别。
	if oaiResp.Error != nil {
		return nil, fmt.Errorf("deepseek: API error: %s", oaiResp.Error.Message)
	}

	// 协议约定至少返回一个 choice；空响应视为异常。
	if len(oaiResp.Choices) == 0 {
		return nil, fmt.Errorf("deepseek: empty response")
	}

	// 只取第一个 choice（框架不使用多候选），回填为 contract.ChatResponse。
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
// 中文：流式对话。请求组装与 Chat 一致，只额外打开 stream 开关并请求 usage 统计；
// 立即返回一个由后台 goroutine 持续写入的只读通道：文本增量即时下发，
// tool_calls 参数按 Index 跨块拼接，直到 [DONE] 哨兵到达时随终止块一次性交付。
func (c *Client) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	// Convert messages (same as Chat).
	// 中文：消息映射与 Chat 完全相同——角色/内容/tool_call_id 直接透传，
	// assistant 消息的历史 ToolCalls 回放给服务端以续接多轮工具调用。
	msgs := make([]oaiMessage, len(req.Messages)) // 一次分配，与输入一一对应
	for i, m := range req.Messages {
		// 基础字段映射。
		msgs[i] = oaiMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		// 回放历史工具调用：type 固定 "function"，参数原始 JSON 不解析。
		for _, tc := range m.ToolCalls {
			msgs[i].ToolCalls = append(msgs[i].ToolCalls, oaiToolCall{
				ID:   tc.ID,      // 沿用原调用 ID，供服务端与 tool 消息配对
				Type: "function", // 协议固定值
				// 匿名结构体字面量需复述 JSON tag，与 oaiToolCall.Function 定义一致。
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: tc.Name, Arguments: tc.Args}, // 工具名 + 原始参数 JSON
			})
		}
	}

	// 工具定义映射（与 Chat 相同）：ToolDef -> function 定义，schema 原样下发。
	var tools []oaiTool
	for _, t := range req.Tools {
		tools = append(tools, oaiTool{
			Type: "function", // 协议固定值
			Function: oaiToolFunction{
				Name:        t.Name,        // 工具名
				Description: t.Description, // 工具用途描述
				Parameters:  t.InputSchema, // 参数 JSON Schema 原样透传
			},
		})
	}

	// 模型名兜底：未指定时使用 deepseek-chat。
	model := req.Model
	if model == "" {
		model = "deepseek-chat"
	}

	// 组装 wire 请求：与 Chat 的差异仅两处——
	// Stream=true 切换 SSE；StreamOptions 请求服务端在流尾补发 usage 统计块。
	oaiReq := oaiRequest{
		Model:         model,                                 // 已兜底的模型名
		Messages:      msgs,                                  // 映射好的消息列表
		Tools:         tools,                                 // 映射好的工具定义
		MaxTokens:     req.MaxTokens,                         // 输出上限
		Temperature:   req.Temperature,                       // nil 表示服务端默认
		Stream:        true,                                  // 开启 SSE 流式返回
		StreamOptions: &oaiStreamOptions{IncludeUsage: true}, // 让服务端在流末尾附带 usage
	}
	// 结构化输出：同 Chat，DeepSeek 只支持 json_object 模式。
	if req.Schema != nil {
		oaiReq.ResponseFormat = &oaiResponseFormat{Type: "json_object"}
	}

	// 序列化请求体。
	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("deepseek: marshal request: %w", err)
	}

	// 构造请求并绑定 ctx：取消 ctx 会中断后续的流读取，使读循环自然退出。
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err // URL 构造失败
	}
	httpReq.Header.Set("Content-Type", "application/json")  // 请求体仍是 JSON
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey) // Bearer 鉴权
	httpReq.Header.Set("Accept", "text/event-stream")       // 声明期望 SSE 流式响应

	// 发起请求；成功后响应体是一条保持打开的 SSE 长连接。
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("deepseek: http: %w", err)
	}

	// 非 200 说明流根本没有建立：读出错误体、关闭连接，同步返回错误。
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(resp.Body) // 错误详情尽力而为，读失败也不影响主错误
		return nil, fmt.Errorf("deepseek: HTTP %d: %s", resp.StatusCode, string(errBody))
	}

	// 带缓冲的增量块通道：允许解析端小幅领先消费端，降低相互阻塞。
	ch := make(chan contract.StreamChunk, 32)

	// 后台 goroutine 承担整个 SSE 解析生命周期；本函数随即返回通道。
	go func() {
		defer resp.Body.Close() // 退出时释放连接
		defer close(ch)         // 关闭通道即向消费方宣告流结束（含异常提前结束）

		// Accumulate tool calls by index across chunks.
		// 中文：tool_calls 的参数 JSON 被拆散在多个增量块里，
		// 以 choice 内的 Index 为键在此累积拼接，直到流结束才完整。
		toolCallMap := make(map[int]*contract.ToolCall)
		var lastUsage *contract.Usage // 暂存流尾 usage 块，随最终 Done 块一并交付

		// 按行扫描 SSE 流：每个事件是一行 "data: {...}"，空行为事件分隔符。
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text() // 当前行文本（不含换行符）

			// 只处理 "data: " 载荷行；空行、注释、其他 SSE 字段一律跳过。
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ") // 剥掉前缀得到 JSON 载荷
			// "[DONE]" 是流结束哨兵（非 JSON），到达即收尾。
			if data == "[DONE]" {
				// Flush accumulated tool calls.
				// 中文：把累积完成的工具调用按 Index 0..n-1 依序还原为切片，
				// 与 Done 标志、usage 统计一起放进最后一个增量块交付。
				if len(toolCallMap) > 0 {
					var tcs []contract.ToolCall
					// 服务端按 0 起连续编号 Index，计数循环保证输出顺序稳定。
					for i := 0; i < len(toolCallMap); i++ {
						if tc, ok := toolCallMap[i]; ok {
							tcs = append(tcs, *tc)
						}
					}
					ch <- contract.StreamChunk{ToolCalls: tcs, Done: true, Usage: lastUsage}
				} else {
					// 纯文本流：只发终止块（Done + usage）。
					ch <- contract.StreamChunk{Done: true, Usage: lastUsage}
				}
				return // 正常收尾：defer 负责关闭通道与连接
			}

			// 反序列化单个增量块；个别畸形块直接跳过，保证整条流的健壮性。
			var chunk oaiStreamChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}

			// Capture usage from any chunk (DeepSeek sends it in a
			// dedicated chunk with empty choices when stream_options.include_usage=true).
			// 中文：usage 统计在流末尾一个 choices 为空的专用块中到达；
			// 这里对任意块做捕获并暂存，等 [DONE] 时随终止块下发。
			if chunk.Usage != nil {
				lastUsage = &contract.Usage{
					InputTokens:  chunk.Usage.PromptTokens,     // prompt_tokens -> 输入 token
					OutputTokens: chunk.Usage.CompletionTokens, // completion_tokens -> 输出 token
				}
			}

			// choices 为空的块（如纯 usage 块）没有增量内容，跳过。
			if len(chunk.Choices) == 0 {
				continue
			}

			// 只消费第一个 choice 的增量（框架不使用多候选）。
			delta := chunk.Choices[0].Delta

			// Content delta.
			// 中文：文本增量即时透传给消费方，实现逐字输出。
			if delta.Content != "" {
				ch <- contract.StreamChunk{Content: delta.Content}
			}

			// Tool call deltas — accumulate by index.
			// 中文：工具调用增量——首个分片携带 ID/Name 与参数开头，后续分片只带参数续片。
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
					// Append to existing (streaming args).
					// 中文：后续分片——把参数 JSON 续片追加到已登记的调用上。
					toolCallMap[idx].Args += tc.Function.Arguments
				}
			}
		}
	}()

	return ch, nil // 立即返回通道，解析在后台进行
}
