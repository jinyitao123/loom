package main

import (
	"context" // 向底层 LLM 透传取消/超时上下文
	"strings" // strings.Builder：累积流式文本增量

	"github.com/jinyitao123/loom/contract" // LLM / ChatRequest / StreamChunk 等契约类型
)

// streamingLLM wraps a contract.LLM so that Chat() drives the provider's token
// Stream and emits each text delta on the sink as it arrives — turning loom's
// blocking tool-loop into a streaming one without touching the engine. If the
// provider can't stream, it falls back to a blocking Chat and emits the whole
// message as a single text event. (This mirrors the old Go Weave
// StreamingLLMAdapter, but targets stdout-json instead of SSE.)
// 中文补充：streamingLLM 包装一个 contract.LLM，让 Chat() 实际改走底层的
// token Stream，并把每个文本增量即时写到 sink（NDJSON 事件流）——在完全不
// 改引擎的前提下，把 loom 的阻塞式工具循环变成流式输出。若底层 provider
// 不支持流式，则回退为阻塞 Chat，把整条消息作为单个 text 事件一次性发出。
// （思路沿袭旧版 Go Weave 的 StreamingLLMAdapter，但输出目标从 SSE 换成 stdout-json。）
type streamingLLM struct {
	inner contract.LLM // 被包装的真实 LLM（provider 实现）
	out   sink         // NDJSON 事件输出端：文本增量实时写往这里
}

// Stream 直接透传给底层 LLM：包装层只改写 Chat 的行为，不干预原生流式接口。
func (s *streamingLLM) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return s.inner.Stream(ctx, req)
}

// Chat 是引擎工具循环的调用入口：内部改用 Stream 驱动，边收边发 text 事件，
// 最终把增量重组成完整的 ChatResponse 交还引擎——引擎对此无感知。
func (s *streamingLLM) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	// 优先尝试建立流式通道。
	ch, err := s.inner.Stream(ctx, req)
	if err != nil {
		// Provider doesn't support streaming — fall back to a blocking call and
		// emit the whole assistant message at once.
		// 中文补充：底层不支持流式（或建流失败）——降级为阻塞 Chat，
		// 并把整条 assistant 消息作为单个 text 事件一次性发出。
		resp, cerr := s.inner.Chat(ctx, req)
		if cerr != nil {
			return nil, cerr // 降级路径也失败：把 Chat 的错误原样上抛
		}
		// 空内容（例如纯工具调用的轮次）不发空 text 事件。
		if resp.Content != "" {
			_ = s.out.Text(resp.Content) // 事件发送失败不影响对话结果，忽略返回值
		}
		return resp, nil
	}

	// 聚合缓冲：流结束后需把所有增量重组成一份完整响应交还引擎。
	var content strings.Builder       // 累积全部文本增量
	var toolCalls []contract.ToolCall // 收集流中出现的工具调用
	var usage contract.Usage          // 记录最终 token 用量
	// 逐块消费流；channel 关闭即代表流结束。
	for chunk := range ch {
		if chunk.Content != "" {
			content.WriteString(chunk.Content) // 先入缓冲，保证最终响应内容完整
			_ = s.out.Text(chunk.Content)      // 再即时外发增量；发送失败不打断消费
		}
		// 工具调用可能分多块到达，按到达顺序追加。
		if len(chunk.ToolCalls) > 0 {
			toolCalls = append(toolCalls, chunk.ToolCalls...)
		}
		// Usage 通常只在末块出现；以最后一次为准整体覆盖。
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
	}
	// 把流式增量重组为与阻塞 Chat 等价的完整响应返回给引擎。
	return &contract.ChatResponse{Content: content.String(), ToolCalls: toolCalls, Usage: usage}, nil
}
