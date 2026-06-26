package main

import (
	"context"
	"strings"

	"github.com/jinyitao123/loom/contract"
)

// streamingLLM wraps a contract.LLM so that Chat() drives the provider's token
// Stream and emits each text delta on the sink as it arrives — turning loom's
// blocking tool-loop into a streaming one without touching the engine. If the
// provider can't stream, it falls back to a blocking Chat and emits the whole
// message as a single text event. (This mirrors the old Go Weave
// StreamingLLMAdapter, but targets stdout-json instead of SSE.)
type streamingLLM struct {
	inner contract.LLM
	out   sink
}

func (s *streamingLLM) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return s.inner.Stream(ctx, req)
}

func (s *streamingLLM) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	ch, err := s.inner.Stream(ctx, req)
	if err != nil {
		// Provider doesn't support streaming — fall back to a blocking call and
		// emit the whole assistant message at once.
		resp, cerr := s.inner.Chat(ctx, req)
		if cerr != nil {
			return nil, cerr
		}
		if resp.Content != "" {
			_ = s.out.Text(resp.Content)
		}
		return resp, nil
	}

	var content strings.Builder
	var toolCalls []contract.ToolCall
	var usage contract.Usage
	for chunk := range ch {
		if chunk.Content != "" {
			content.WriteString(chunk.Content)
			_ = s.out.Text(chunk.Content)
		}
		if len(chunk.ToolCalls) > 0 {
			toolCalls = append(toolCalls, chunk.ToolCalls...)
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
	}
	return &contract.ChatResponse{Content: content.String(), ToolCalls: toolCalls, Usage: usage}, nil
}
