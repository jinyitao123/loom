package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

// sse renders events as a server-sent event stream.
func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(e), &head)
		b.WriteString("event: " + head.Type + "\ndata: " + e + "\n\n")
	}
	return b.String()
}

// server answers every request with status and body, recording the requests.
func server(t *testing.T, status int, body string) (*Client, *[]map[string]any, *[]http.Header) {
	t.Helper()
	var bodies []map[string]any
	var headers []http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		data, _ := io.ReadAll(r.Body)
		var v map[string]any
		if err := json.Unmarshal(data, &v); err != nil {
			t.Errorf("request is not JSON: %s", data)
		}
		bodies = append(bodies, v)
		headers = append(headers, r.Header.Clone())
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	return New("key-1", WithBaseURL(ts.URL+"/v1"), WithDefaultModel("claude-test")), &bodies, &headers
}

var thinkingAndTool = sse(
	`{"type":"message_start","message":{"usage":{"input_tokens":12,"cache_creation_input_tokens":100,"cache_read_input_tokens":400,"output_tokens":1}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Check the "}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"file."}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"opaque"}}`,
	`{"type":"content_block_stop","index":1}`,
	`{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"Reading it."}}`,
	`{"type":"content_block_stop","index":2}`,
	`{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_1","name":"read","input":{}}}`,
	`{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
	`{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"\"a.go\"}"}}`,
	`{"type":"content_block_stop","index":3}`,
	`{"type":"ping"}`,
	`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":40}}`,
	`{"type":"message_stop"}`,
)

func TestStreamThinkingTextAndToolCall(t *testing.T) {
	client, _, headers := server(t, 200, thinkingAndTool)
	ch, err := client.Stream(context.Background(), contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var reasoning, text []string
	var blocks []contract.Reasoning
	var done *contract.StreamChunk
	for chunk := range ch {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		if chunk.Reasoning != "" {
			reasoning = append(reasoning, chunk.Reasoning)
		}
		if chunk.Content != "" {
			text = append(text, chunk.Content)
		}
		if chunk.ReasoningBlock != nil {
			blocks = append(blocks, *chunk.ReasoningBlock)
		}
		if chunk.Done {
			c := chunk
			done = &c
		}
	}
	if strings.Join(reasoning, "") != "Check the file." || strings.Join(text, "") != "Reading it." {
		t.Fatalf("reasoning %q, text %q", reasoning, text)
	}
	want := []contract.Reasoning{{Text: "Check the file.", Signature: "sig-abc"}, {Signature: "opaque", Redacted: true}}
	if len(blocks) != 2 || blocks[0] != want[0] || blocks[1] != want[1] {
		t.Fatalf("blocks = %+v", blocks)
	}
	if done == nil || done.FinishReason != "tool_calls" || len(done.ToolCalls) != 1 {
		t.Fatalf("done = %+v", done)
	}
	if call := done.ToolCalls[0]; call.ID != "toolu_1" || call.Name != "read" || call.Args != `{"path":"a.go"}` {
		t.Fatalf("call = %+v", call)
	}
	if u := done.Usage; u.InputTokens != 512 || u.CacheReadTokens != 400 || u.CacheWriteTokens != 100 || u.OutputTokens != 40 {
		t.Fatalf("usage = %+v", u)
	}
	h := (*headers)[0]
	if h.Get("x-api-key") != "key-1" || h.Get("anthropic-version") != APIVersion {
		t.Fatalf("headers = %v", h)
	}
}

func TestChatAssemblesTheResponse(t *testing.T) {
	client, _, _ := server(t, 200, thinkingAndTool)
	resp, err := client.Chat(context.Background(), contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Reading it." || len(resp.Reasoning) != 2 || len(resp.ToolCalls) != 1 || resp.StopReason != "tool_calls" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestRequestCarriesHistoryThinkingAndCacheMarks(t *testing.T) {
	client, bodies, _ := server(t, 200, sse(
		`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"ok"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	))
	temp := 0.3
	req := contract.ChatRequest{
		Temperature: &temp,
		MaxTokens:   20000,
		Effort:      contract.EffortMedium,
		Tools: []contract.ToolDef{
			{Name: "read", Description: "Read a file", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
			{Name: "ls"},
		},
		Messages: []contract.Message{
			{Role: "system", Content: "Be brief."},
			{Role: "user", Content: "look", Parts: []contract.ContentPart{{Type: "text", Text: "look"}, {Type: "image", MediaType: "image/png", Data: "AAAA"}}},
			{Role: "assistant", Content: "", Reasoning: []contract.Reasoning{
				{Text: "plan", Signature: "s1"}, {Signature: "r1", Redacted: true}, {Text: "foreign, unsigned"},
			}, ToolCalls: []contract.ToolCall{{ID: "t1", Name: "read", Args: `{"path":"a"}`}, {ID: "t2", Name: "ls", Args: ""}}},
			{Role: "tool", ToolCallID: "t1", Content: "file a"},
			{Role: "tool", ToolCallID: "t2", Content: "", Parts: []contract.ContentPart{{Type: "image", MediaType: "image/png", Data: "BBBB"}}},
			{Role: "user", Content: "and b?"},
		},
	}
	if _, err := client.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal((*bodies)[0])
	var wire request
	if err := json.Unmarshal(got, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Model != "claude-test" || !wire.Stream || wire.MaxTokens != 20000 {
		t.Fatalf("wire = %s", got)
	}
	if wire.Thinking == nil || wire.Thinking.BudgetTokens != ThinkingBudgets[contract.EffortMedium] || wire.Temperature != nil {
		t.Fatalf("thinking must be on and temperature dropped: %s", got)
	}
	if len(wire.System) != 1 || wire.System[0].Text != "Be brief." || wire.System[0].CacheControl == nil {
		t.Fatalf("system = %+v", wire.System)
	}
	if len(wire.Tools) != 2 || wire.Tools[1].CacheControl == nil || wire.Tools[0].CacheControl != nil || string(wire.Tools[1].InputSchema) != `{"type":"object"}` {
		t.Fatalf("tools = %s", got)
	}
	if len(wire.Messages) != 3 {
		t.Fatalf("messages must alternate user/assistant/user: %s", got)
	}
	user, assistant, results := wire.Messages[0], wire.Messages[1], wire.Messages[2]
	if user.Content[1].Type != "image" || user.Content[1].Source.MediaType != "image/png" {
		t.Fatalf("user = %+v", user)
	}
	kinds := []string{}
	for _, b := range assistant.Content {
		kinds = append(kinds, b.Type)
	}
	if strings.Join(kinds, ",") != "thinking,redacted_thinking,tool_use,tool_use" {
		t.Fatalf("assistant blocks = %v (unsigned reasoning must be dropped)", kinds)
	}
	if assistant.Content[0].Signature != "s1" || assistant.Content[1].Data != "r1" || string(assistant.Content[3].Input) != `{}` {
		t.Fatalf("assistant = %s", got)
	}
	if results.Role != "user" || len(results.Content) != 3 || results.Content[0].Type != "tool_result" || results.Content[1].Content[0].Type != "image" || results.Content[2].Text != "and b?" {
		t.Fatalf("tool results and the next user text form one turn: %s", got)
	}
	if results.Content[2].CacheControl == nil {
		t.Fatalf("the newest block must close the cached prefix: %s", got)
	}
}

func TestThinkingIsOffForForcedToolsAndSmallLimits(t *testing.T) {
	c := New("k", WithDefaultModel("m"))
	msgs := []contract.Message{{Role: "user", Content: "x"}}
	forced, _ := c.buildRequest(contract.ChatRequest{Messages: msgs, Effort: contract.EffortHigh,
		Tools: []contract.ToolDef{{Name: "a"}}, ToolChoice: &contract.ToolChoice{Mode: contract.ToolChoiceRequired}})
	if forced.Thinking != nil || forced.ToolChoice.Type != "any" {
		t.Fatalf("forced = %+v", forced)
	}
	small, _ := c.buildRequest(contract.ChatRequest{Messages: msgs, Effort: contract.EffortHigh, MaxTokens: 4000})
	if small.Thinking == nil || small.Thinking.BudgetTokens != 2000 {
		t.Fatalf("budget must stay under the limit: %+v", small.Thinking)
	}
	off, _ := c.buildRequest(contract.ChatRequest{Messages: msgs})
	if off.Thinking != nil || off.MaxTokens != DefaultMaxTokens {
		t.Fatalf("off = %+v", off)
	}
}

func TestErrorsAreReported(t *testing.T) {
	client, _, _ := server(t, 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	_, err := client.Chat(context.Background(), contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "HTTP 529") || !strings.Contains(err.Error(), "overloaded_error") {
		t.Fatalf("err = %v", err)
	}

	client, _, _ = server(t, 200, sse(
		`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
		`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
	))
	if _, err = client.Chat(context.Background(), contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: "x"}}}); err == nil || !strings.Contains(err.Error(), "overloaded") {
		t.Fatalf("stream error = %v", err)
	}

	client, _, _ = server(t, 200, sse(
		`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"partial"}}`,
	))
	if _, err = client.Chat(context.Background(), contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: "x"}}}); err == nil || !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("truncated stream = %v", err)
	}

	client, _, _ = server(t, 200, sse(
		`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`,
		`{"type":"message_stop"}`,
	))
	if _, err = client.Chat(context.Background(), contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: "x"}}}); !errors.Is(err, ErrEmptyResponse) {
		t.Fatalf("empty = %v", err)
	}
}

func TestStopReasons(t *testing.T) {
	for in, want := range map[string]string{"end_turn": "stop", "stop_sequence": "stop", "tool_use": "tool_calls", "max_tokens": "length", "refusal": "stop"} {
		if got := stopReason(in); got != want {
			t.Errorf("stopReason(%q) = %q, want %q", in, got, want)
		}
	}
}
