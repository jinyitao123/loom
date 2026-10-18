package openai_test

import (
	"testing"

	"github.com/jinyitao123/loom/contract"
)

func TestChatReturnsReasoningAndCachedTokens(t *testing.T) {
	resp, err := chatWithServer(t, `{"choices":[{"message":{"role":"assistant","content":"42","reasoning_content":"think first"},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":80}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Reasoning) != 1 || resp.Reasoning[0].Text != "think first" {
		t.Fatalf("reasoning = %+v", resp.Reasoning)
	}
	if resp.Usage.InputTokens != 100 || resp.Usage.CacheReadTokens != 80 || resp.Usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if msg := resp.AsMessage(); len(msg.Reasoning) != 1 {
		t.Fatalf("assistant message lost its reasoning: %+v", msg)
	}

	// Gateways that name the field "reasoning" are read as well.
	resp, err = chatWithServer(t, `{"choices":[{"message":{"role":"assistant","content":"ok","reasoning":"via gateway"},"finish_reason":"stop"}]}`)
	if err != nil || len(resp.Reasoning) != 1 || resp.Reasoning[0].Text != "via gateway" {
		t.Fatalf("gateway reasoning = %+v, %v", resp, err)
	}
	resp, err = chatWithServer(t, `{"choices":[{"message":{"role":"assistant","content":"plain"},"finish_reason":"stop"}]}`)
	if err != nil || resp.Reasoning != nil {
		t.Fatalf("a response without reasoning must not invent it: %+v, %v", resp, err)
	}
}

func TestStreamDeliversReasoningDeltasThenTheBlock(t *testing.T) {
	chunks := streamWithServer(t,
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"step one; \"},\"finish_reason\":null}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"step two\"},\"finish_reason\":null}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":9,\"prompt_tokens_details\":{\"cached_tokens\":32}}}\n\n"+
			"data: [DONE]\n\n")
	var deltas []string
	var terminal *contract.StreamChunk
	for i := range chunks {
		if chunks[i].Err != nil {
			t.Fatal(chunks[i].Err)
		}
		if chunks[i].Reasoning != "" {
			deltas = append(deltas, chunks[i].Reasoning)
		}
		if chunks[i].ReasoningBlock != nil && !chunks[i].Done {
			t.Fatalf("the reasoning block must come with the terminal chunk: %+v", chunks[i])
		}
		if chunks[i].Done {
			terminal = &chunks[i]
		}
	}
	if len(deltas) != 2 || deltas[0] != "step one; " || deltas[1] != "step two" {
		t.Fatalf("reasoning deltas = %q", deltas)
	}
	if terminal == nil || terminal.ReasoningBlock == nil || terminal.ReasoningBlock.Text != "step one; step two" {
		t.Fatalf("terminal = %+v", terminal)
	}
	if terminal.Usage == nil || terminal.Usage.CacheReadTokens != 32 || terminal.Usage.InputTokens != 50 {
		t.Fatalf("usage = %+v", terminal.Usage)
	}
}

func TestStreamWithoutReasoningHasNoBlock(t *testing.T) {
	chunks := streamWithServer(t,
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: [DONE]\n\n")
	for _, chunk := range chunks {
		if chunk.Reasoning != "" || chunk.ReasoningBlock != nil {
			t.Fatalf("unexpected reasoning: %+v", chunk)
		}
	}
}
