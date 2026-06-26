package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

// streamLLM is a fake LLM whose Stream emits scripted chunks (or errors,
// triggering the blocking-Chat fallback).
type streamLLM struct {
	chunks    []contract.StreamChunk
	streamErr error
	chatResp  contract.ChatResponse
	chatErr   error
}

func (s *streamLLM) Stream(_ context.Context, _ contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	if s.streamErr != nil {
		return nil, s.streamErr
	}
	ch := make(chan contract.StreamChunk)
	go func() {
		defer close(ch)
		for _, c := range s.chunks {
			ch <- c
		}
	}()
	return ch, nil
}

func (s *streamLLM) Chat(_ context.Context, _ contract.ChatRequest) (*contract.ChatResponse, error) {
	if s.chatErr != nil {
		return nil, s.chatErr
	}
	r := s.chatResp
	return &r, nil
}

func TestStreamingLLMStreamsDeltas(t *testing.T) {
	var buf bytes.Buffer
	inner := &streamLLM{chunks: []contract.StreamChunk{{Content: "Hello"}, {Content: " world"}, {Done: true}}}
	s := &streamingLLM{inner: inner, out: NewEmitter(&buf)}

	resp, err := s.Chat(context.Background(), contract.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Hello world" {
		t.Fatalf("accumulated content = %q", resp.Content)
	}
	lines := parseLines(t, &buf)
	var texts []string
	for _, l := range lines {
		if l["type"] == "text" {
			texts = append(texts, l["text"].(string))
		}
	}
	if len(texts) != 2 || texts[0] != "Hello" || texts[1] != " world" {
		t.Fatalf("expected 2 streamed text deltas, got %v", texts)
	}
}

func TestStreamingLLMFallsBackToChat(t *testing.T) {
	var buf bytes.Buffer
	inner := &streamLLM{streamErr: errors.New("no stream"), chatResp: contract.ChatResponse{Content: "blocking answer"}}
	s := &streamingLLM{inner: inner, out: NewEmitter(&buf)}

	resp, err := s.Chat(context.Background(), contract.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "blocking answer" {
		t.Fatalf("content = %q", resp.Content)
	}
	if !hasEvent(parseLines(t, &buf), "text", "text", "blocking answer") {
		t.Fatalf("fallback should emit the whole content as one text event")
	}
}

func TestStreamingLLMAccumulatesToolCalls(t *testing.T) {
	var buf bytes.Buffer
	inner := &streamLLM{chunks: []contract.StreamChunk{{ToolCalls: []contract.ToolCall{{ID: "c1", Name: "t"}}}, {Done: true}}}
	s := &streamingLLM{inner: inner, out: NewEmitter(&buf)}

	resp, err := s.Chat(context.Background(), contract.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "c1" {
		t.Fatalf("tool calls not accumulated: %+v", resp.ToolCalls)
	}
}

func TestStreamingLLMFallbackPropagatesChatError(t *testing.T) {
	var buf bytes.Buffer
	inner := &streamLLM{streamErr: errors.New("no stream"), chatErr: errors.New("upstream 500")}
	s := &streamingLLM{inner: inner, out: NewEmitter(&buf)}
	if _, err := s.Chat(context.Background(), contract.ChatRequest{}); err == nil {
		t.Fatal("want chat error propagated")
	}
}
