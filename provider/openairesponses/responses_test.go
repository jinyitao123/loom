package openairesponses

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

func server(t *testing.T, status int, body string) (*Client, *[]map[string]any, *[]http.Header) {
	t.Helper()
	var bodies []map[string]any
	var headers []http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
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
	return New("key-1", WithBaseURL(ts.URL+"/v1/"), WithDefaultModel("gpt-test")), &bodies, &headers
}

var reasoningAndCall = sse(
	`{"type":"response.created","response":{"status":"in_progress"}}`,
	`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`,
	`{"type":"response.reasoning_summary_part.added","output_index":0}`,
	`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"Find "}`,
	`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"the file."}`,
	`{"type":"response.reasoning_summary_part.added","output_index":0}`,
	`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"Then read it."}`,
	`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc-1","summary":[{"type":"summary_text","text":"Find the file."},{"type":"summary_text","text":"Then read it."}]}}`,
	`{"type":"response.output_text.delta","output_index":1,"delta":"Reading."}`,
	`{"type":"response.output_item.done","output_index":1,"item":{"type":"message"}}`,
	`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"path\""}`,
	`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"path\":\"a.go\"}"}}`,
	`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":900,"input_tokens_details":{"cached_tokens":700},"output_tokens":30}}}`,
)

func TestStreamReasoningTextAndCall(t *testing.T) {
	client, _, headers := server(t, 200, reasoningAndCall)
	ch, err := client.Stream(context.Background(), contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var reasoning, text strings.Builder
	var blocks []contract.Reasoning
	var done *contract.StreamChunk
	for chunk := range ch {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		reasoning.WriteString(chunk.Reasoning)
		text.WriteString(chunk.Content)
		if chunk.ReasoningBlock != nil {
			blocks = append(blocks, *chunk.ReasoningBlock)
		}
		if chunk.Done {
			c := chunk
			done = &c
		}
	}
	if reasoning.String() != "Find the file.\n\nThen read it." || text.String() != "Reading." {
		t.Fatalf("reasoning %q text %q", reasoning.String(), text.String())
	}
	if len(blocks) != 1 || blocks[0].Signature != "enc-1" || blocks[0].Text != "Find the file.\n\nThen read it." {
		t.Fatalf("blocks = %+v", blocks)
	}
	if done == nil || done.FinishReason != "tool_calls" || len(done.ToolCalls) != 1 || done.ToolCalls[0].ID != "call_1" || done.ToolCalls[0].Args != `{"path":"a.go"}` {
		t.Fatalf("done = %+v", done)
	}
	if u := done.Usage; u.InputTokens != 900 || u.CacheReadTokens != 700 || u.OutputTokens != 30 {
		t.Fatalf("usage = %+v", u)
	}
	if (*headers)[0].Get("Authorization") != "Bearer key-1" {
		t.Fatalf("auth = %v", (*headers)[0])
	}
}

func TestRequestShape(t *testing.T) {
	client, bodies, _ := server(t, 200, sse(
		`{"type":"response.output_text.delta","output_index":0,"delta":"ok"}`,
		`{"type":"response.completed","response":{"status":"completed"}}`,
	))
	temp := 0.2
	_, err := client.Chat(context.Background(), contract.ChatRequest{
		Temperature: &temp, MaxTokens: 5000, Effort: contract.EffortHigh,
		Tools: []contract.ToolDef{{Name: "read", Description: "Read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []contract.Message{
			{Role: "system", Content: "Be brief."},
			{Role: "user", Content: "look", Parts: []contract.ContentPart{{Type: "text", Text: "look"}, {Type: "image", MediaType: "image/png", Data: "AAAA"}}},
			{Role: "assistant", Content: "Let me read.", Reasoning: []contract.Reasoning{{Text: "plan", Signature: "enc-1"}, {Text: "foreign, unsigned"}},
				ToolCalls: []contract.ToolCall{{ID: "call_1", Name: "read", Args: `{"path":"a"}`}}},
			{Role: "tool", ToolCallID: "call_1", Content: "file a"},
			{Role: "tool", ToolCallID: "call_2", Parts: []contract.ContentPart{{Type: "image", MediaType: "image/png", Data: "BBBB"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := (*bodies)[0]
	if body["store"] != false || body["stream"] != true || body["instructions"] != "Be brief." || body["max_output_tokens"] != float64(5000) || body["temperature"] != nil {
		t.Fatalf("body = %v", body)
	}
	if r := body["reasoning"].(map[string]any); r["effort"] != "high" || r["summary"] != "auto" {
		t.Fatalf("reasoning = %v", r)
	}
	if inc := body["include"].([]any); len(inc) != 1 || inc[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %v", inc)
	}
	input := body["input"].([]any)
	var kinds []string
	for _, item := range input {
		m := item.(map[string]any)
		kind, _ := m["type"].(string)
		if kind == "" {
			kind = m["role"].(string)
		}
		kinds = append(kinds, kind)
	}
	if got := strings.Join(kinds, ","); got != "user,reasoning,assistant,function_call,function_call_output,function_call_output" {
		t.Fatalf("input kinds = %s", got)
	}
	user := input[0].(map[string]any)["content"].([]any)
	if user[1].(map[string]any)["image_url"] != "data:image/png;base64,AAAA" {
		t.Fatalf("user = %v", user)
	}
	reasoning := input[1].(map[string]any)
	if reasoning["encrypted_content"] != "enc-1" || reasoning["id"] != nil || reasoning["summary"].([]any)[0].(map[string]any)["text"] != "plan" {
		t.Fatalf("reasoning item = %v", reasoning)
	}
	if out := input[4].(map[string]any); out["output"] != "file a" || out["call_id"] != "call_1" {
		t.Fatalf("output = %v", out)
	}
	if out := input[5].(map[string]any)["output"].([]any); out[0].(map[string]any)["type"] != "input_image" {
		t.Fatalf("image output = %v", out)
	}
	tools := body["tools"].([]any)
	if tool := tools[0].(map[string]any); tool["type"] != "function" || tool["name"] != "read" {
		t.Fatalf("tools = %v", tools)
	}
}

func TestStopsErrorsAndEmpty(t *testing.T) {
	client, _, _ := server(t, 200, sse(
		`{"type":"response.output_text.delta","output_index":0,"delta":"cut"}`,
		`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
	))
	resp, err := client.Chat(context.Background(), contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: "x"}}})
	if err != nil || resp.StopReason != "length" || resp.Content != "cut" {
		t.Fatalf("incomplete = %+v, %v", resp, err)
	}

	client, _, _ = server(t, 429, `{"error":{"type":"requests","code":"rate_limit_exceeded","message":"Rate limit reached"}}`)
	if _, err = client.Chat(context.Background(), contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: "x"}}}); err == nil || !strings.Contains(err.Error(), "HTTP 429") {
		t.Fatalf("429 = %v", err)
	}

	client, _, _ = server(t, 200, sse(`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"boom"}}}`))
	if _, err = client.Chat(context.Background(), contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: "x"}}}); err == nil || !strings.Contains(err.Error(), "server_error") {
		t.Fatalf("failed = %v", err)
	}

	client, _, _ = server(t, 200, sse(`{"type":"response.output_text.delta","output_index":0,"delta":"partial"}`))
	if _, err = client.Chat(context.Background(), contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: "x"}}}); err == nil || !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("truncated = %v", err)
	}

	client, _, _ = server(t, 200, sse(`{"type":"response.completed","response":{"status":"completed"}}`))
	if _, err = client.Chat(context.Background(), contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: "x"}}}); !errors.Is(err, ErrEmptyResponse) {
		t.Fatalf("empty = %v", err)
	}
}

func TestNoReasoningWithoutEffort(t *testing.T) {
	c := New("k", WithDefaultModel("m"))
	temp := 0.5
	r, err := c.buildRequest(contract.ChatRequest{Temperature: &temp, Messages: []contract.Message{{Role: "user", Content: "x"}}})
	if err != nil || r.Reasoning != nil || r.Include != nil || r.Temperature == nil {
		t.Fatalf("request = %+v, %v", r, err)
	}
}
