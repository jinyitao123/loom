package stdlib

import (
	"context"
	"strings"

	"github.com/jinyitao123/loom/contract"
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
func NewStreamingLLM(inner contract.LLM, sink contract.StreamSink) contract.LLM {
	return &streamingLLM{inner: inner, sink: sink}
}

type streamingLLM struct {
	inner contract.LLM
	sink  contract.StreamSink
}

func (s *streamingLLM) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return s.inner.Stream(ctx, req)
}

func (s *streamingLLM) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	ch, err := s.inner.Stream(ctx, req)
	if err != nil {
		resp, cerr := s.inner.Chat(ctx, req)
		if cerr != nil {
			return nil, cerr
		}
		if resp.Content != "" {
			_ = s.sink.SendChunk(contract.StreamChunk{Content: resp.Content})
		}
		_ = s.sink.SendChunk(contract.StreamChunk{Done: true, Usage: &resp.Usage})
		return resp, nil
	}

	var content strings.Builder
	var toolCalls []contract.ToolCall
	var usage contract.Usage
	for chunk := range ch {
		if chunk.Content != "" {
			content.WriteString(chunk.Content)
			_ = s.sink.SendChunk(contract.StreamChunk{Content: chunk.Content})
		}
		if len(chunk.ToolCalls) > 0 {
			toolCalls = append(toolCalls, chunk.ToolCalls...)
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
	}
	_ = s.sink.SendChunk(contract.StreamChunk{Done: true, Usage: &usage})
	return &contract.ChatResponse{Content: content.String(), ToolCalls: toolCalls, Usage: usage}, nil
}
