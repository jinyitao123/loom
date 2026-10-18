package stdlib

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

// collectSink records chunks; implements contract.StreamSink.
type collectSink struct {
	chunks []contract.StreamChunk
}

func (c *collectSink) SendChunk(ch contract.StreamChunk) error {
	c.chunks = append(c.chunks, ch)
	return nil
}
func (c *collectSink) SendEvent(string, any) error { return nil }
func (c *collectSink) Close() error                { return nil }

// chunkLLM streams a fixed set of chunks; Chat is the blocking fallback.
type chunkLLM struct {
	chunks    []contract.StreamChunk
	streamErr error
}

func (f *chunkLLM) Chat(_ context.Context, _ contract.ChatRequest) (*contract.ChatResponse, error) {
	return &contract.ChatResponse{Content: "fallback", Usage: contract.Usage{OutputTokens: 1}}, nil
}

func (f *chunkLLM) Stream(_ context.Context, _ contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	ch := make(chan contract.StreamChunk, len(f.chunks))
	for _, c := range f.chunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}

func TestStreamingLLMForwardsDeltas(t *testing.T) {
	sink := &collectSink{}
	llm := NewStreamingLLM(&chunkLLM{chunks: []contract.StreamChunk{
		{Content: "你好"},
		{Content: "，汪"},
		{ToolCalls: []contract.ToolCall{{ID: "t1", Name: "jump"}}, Done: true,
			Usage: &contract.Usage{OutputTokens: 5}},
	}}, sink)

	resp, err := llm.Chat(context.Background(), contract.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "你好，汪" {
		t.Errorf("assembled content = %q", resp.Content)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "jump" {
		t.Errorf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", resp.Usage)
	}

	var text strings.Builder
	doneSeen := false
	for _, c := range sink.chunks {
		text.WriteString(c.Content)
		if c.Done {
			doneSeen = true
		}
	}
	if text.String() != "你好，汪" {
		t.Errorf("sink saw %q", text.String())
	}
	if !doneSeen {
		t.Error("sink never saw a Done chunk")
	}
}

func TestStreamingLLMFallsBackToChat(t *testing.T) {
	sink := &collectSink{}
	llm := NewStreamingLLM(&chunkLLM{streamErr: errors.New("no stream")}, sink)

	resp, err := llm.Chat(context.Background(), contract.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "fallback" {
		t.Errorf("content = %q", resp.Content)
	}
	if len(sink.chunks) < 2 || sink.chunks[0].Content != "fallback" {
		t.Errorf("sink should see the whole message as one chunk, got %+v", sink.chunks)
	}
}

func TestStreamingLLMReturnsFinishReason(t *testing.T) {
	for _, reason := range []string{"stop", "length"} {
		sink := &collectSink{}
		llm := NewStreamingLLM(&chunkLLM{chunks: []contract.StreamChunk{
			{Content: "partial"},
			{Done: true, FinishReason: reason},
		}}, sink)

		resp, err := llm.Chat(context.Background(), contract.ChatRequest{})
		if err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
		if resp.StopReason != reason {
			t.Errorf("StopReason = %q, want %q", resp.StopReason, reason)
		}
		last := sink.chunks[len(sink.chunks)-1]
		if !last.Done || last.FinishReason != reason {
			t.Errorf("%s: final sink chunk = %+v", reason, last)
		}
	}
}

func TestStreamingLLMPropagatesStreamErrors(t *testing.T) {
	probe := errors.New("probe")
	for name, streamErr := range map[string]error{"error": probe, "cancel": context.Canceled} {
		sink := &collectSink{}
		llm := NewStreamingLLM(&chunkLLM{chunks: []contract.StreamChunk{
			{Content: "partial"},
			{Err: streamErr},
		}}, sink)

		resp, err := llm.Chat(context.Background(), contract.ChatRequest{})
		if !errors.Is(err, streamErr) {
			t.Fatalf("%s: err = %v, want %v", name, err, streamErr)
		}
		if resp != nil {
			t.Errorf("%s: partial response returned as success: %+v", name, resp)
		}
		for _, c := range sink.chunks {
			if c.Done {
				t.Errorf("%s: sink saw Done after a stream error", name)
			}
		}
	}
}

func TestStreamingLLMRejectsStreamWithoutDone(t *testing.T) {
	sink := &collectSink{}
	llm := NewStreamingLLM(&chunkLLM{chunks: []contract.StreamChunk{{Content: "partial"}}}, sink)

	resp, err := llm.Chat(context.Background(), contract.ChatRequest{})
	if !errors.Is(err, ErrStreamIncomplete) {
		t.Fatalf("err = %v, want ErrStreamIncomplete", err)
	}
	if resp != nil {
		t.Errorf("partial response returned as success: %+v", resp)
	}
	for _, c := range sink.chunks {
		if c.Done {
			t.Error("sink saw Done for a truncated stream")
		}
	}
}
