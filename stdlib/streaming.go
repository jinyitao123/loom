package stdlib

import (
	"context" // 透传给内层 LLM，承载超时/取消
	"strings" // strings.Builder 增量拼装流式文本，避免反复分配字符串

	"github.com/jinyitao123/loom/contract" // 只依赖 contract 抽象，不接触任何具体 provider
)

// NewStreamingLLM wraps an LLM so that Chat() drives the provider's token
// Stream and forwards each delta to sink as it arrives — turning any
// Chat-based step (e.g. the tool loop) into a streaming one without touching
// the engine. Tool-call deltas stay internal: the wrapped Chat still returns
// a fully assembled ChatResponse, so callers behave identically.
//
// If the provider's Stream fails, it falls back to a blocking Chat and emits
// the whole message as a single chunk, so hosts can rely on the sink seeing
// every assistant utterance either way. A final chunk with Done=true (and
// Usage when the provider reports it) marks the end of each response.
//
// NewStreamingLLM 是装饰器模式的典型运用：包装任意 LLM，使其 Chat 内部改走
// provider 的 Stream 通道，把每个文本增量实时推给 sink，最终仍组装并返回
// 完整的 ChatResponse——调用方（如工具循环）完全无感，引擎零改动即获得流式输出。
// 工具调用增量只在内部累积、不外推；Stream 失败时回退阻塞式 Chat 兜底，
// 保证宿主无论走哪条路径都能从 sink 收到全部 assistant 输出。
func NewStreamingLLM(inner contract.LLM, sink contract.StreamSink) contract.LLM {
	return &streamingLLM{inner: inner, sink: sink} // 返回 contract.LLM 接口，隐藏装饰器细节
}

// streamingLLM 是装饰器本体：持有被包装的 LLM 与宿主提供的流式输出 sink。
type streamingLLM struct {
	inner contract.LLM        // 被包装的真实 LLM（任意 provider）
	sink  contract.StreamSink // 宿主接收流式输出的界面（CLI/WebSocket/SSE 等）
}

// Stream 直接透传给内层实现：显式调 Stream 的调用方自己消费通道，装饰器不插手。
func (s *streamingLLM) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return s.inner.Stream(ctx, req)
}

// Chat 是装饰的核心：表面是阻塞式调用，内部改走 Stream 并逐块推送给 sink。
func (s *streamingLLM) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	// 优先尝试流式通道。
	ch, err := s.inner.Stream(ctx, req)
	if err != nil {
		// 兜底路径：Stream 建立失败（如 provider 不支持流式）时回退阻塞式 Chat，
		// 保证功能可用性优先于流式体验。
		resp, cerr := s.inner.Chat(ctx, req)
		if cerr != nil {
			return nil, cerr // 两条路径都失败才真正报错
		}
		// 把完整回复当作单个大块推给 sink，宿主侧仍能看到全部内容。
		if resp.Content != "" {
			_ = s.sink.SendChunk(contract.StreamChunk{Content: resp.Content})
		}
		// 补发 Done 终止块并附带用量：与流式路径保持相同的收尾协议。
		// sink 推送失败被有意忽略——展示层故障不应影响 LLM 调用结果。
		_ = s.sink.SendChunk(contract.StreamChunk{Done: true, Usage: &resp.Usage})
		return resp, nil
	}

	// 流式路径：一边消费通道一边累积，最终拼装出与阻塞式等价的完整响应。
	var content strings.Builder       // 累积文本增量
	var toolCalls []contract.ToolCall // 累积工具调用片段（不外推，保持内部）
	var usage contract.Usage          // 记录 provider 上报的用量（通常在末尾块）
	for chunk := range ch {           // 通道由 provider 在流结束时关闭，循环自然退出
		if chunk.Content != "" {
			content.WriteString(chunk.Content) // 拼入完整文本
			// 实时把文本增量转发给 sink——这就是"边生成边显示"的来源；
			// 推送失败同样忽略，不让展示层拖垮主流程。
			_ = s.sink.SendChunk(contract.StreamChunk{Content: chunk.Content})
		}
		if len(chunk.ToolCalls) > 0 {
			// 工具调用增量只累积、不推给 sink：它们属于工具循环的内部协议，
			// 宿主看到半截 JSON 参数毫无意义。
			toolCalls = append(toolCalls, chunk.ToolCalls...)
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage // 以最后一次上报为准（provider 通常仅在终止块携带）
		}
	}
	// 流正常结束：发 Done 终止块并附带用量，宿主据此收尾渲染。
	_ = s.sink.SendChunk(contract.StreamChunk{Done: true, Usage: &usage})
	// 返回组装好的完整响应：对调用方而言与直接调 inner.Chat 结果一致。
	return &contract.ChatResponse{Content: content.String(), ToolCalls: toolCalls, Usage: usage}, nil
}
