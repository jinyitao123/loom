package stdlib_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/anthropic"
	"github.com/jinyitao123/loom/provider/openairesponses"
	"github.com/jinyitao123/loom/stdlib"
)

type nativeProtocol struct {
	name, first, last string
	newClient         func(string) contract.LLM
}

func nativeProtocols() []nativeProtocol {
	return []nativeProtocol{
		{"anthropic", nativeSSE(
			`{"type":"message_start","message":{"usage":{"input_tokens":12,"cache_read_input_tokens":0,"cache_creation_input_tokens":8}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque-native"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"b","name":"read"}}`,
			`{"type":"content_block_stop","index":2}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"a","name":"read"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`,
			`{"type":"message_stop"}`), nativeSSE(
			`{"type":"message_start","message":{"usage":{"input_tokens":2}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"done"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			`{"type":"message_stop"}`), func(url string) contract.LLM {
			return anthropic.New("synthetic", anthropic.WithBaseURL(url), anthropic.WithDefaultModel("test"))
		}},
		{"responses", nativeSSE(
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","summary":[{"type":"summary_text","text":"plan"}],"encrypted_content":"opaque-native"}}`,
			`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","call_id":"b","name":"read","arguments":"{}"}}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"a","name":"read","arguments":"{}"}}`,
			`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":20,"output_tokens":4,"input_tokens_details":{"cached_tokens":0}}}}`), nativeSSE(
			`{"type":"response.output_text.delta","delta":"done"}`,
			`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":2,"output_tokens":1}}}`), func(url string) contract.LLM {
			return openairesponses.New("synthetic", openairesponses.WithBaseURL(url), openairesponses.WithDefaultModel("test"))
		}},
	}
}

func nativeSSE(events ...string) string {
	return "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
}

// The first response is durably recorded before a simulated crash. A fresh
// replay must keep the opaque reasoning and known-zero accounting, and must
// not make the physical model request again. The next real request includes
// the recovered reasoning and ordered tool receipts, including image content.
func TestNativeProviderJournalRecovery(t *testing.T) {
	for _, p := range nativeProtocols() {
		t.Run(p.name, func(t *testing.T) {
			var physical atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				if physical.Add(1) == 1 {
					io.WriteString(w, p.first)
				} else {
					for _, value := range []string{"opaque-native", "image/png", "AAAA", "receipt-a", "receipt-b"} {
						if !strings.Contains(string(body), value) {
							t.Errorf("recovered request lost %s", value)
						}
					}
					io.WriteString(w, p.last)
				}
			}))
			defer ts.Close()
			j := &sequenceJournal{crashAt: 0, crashMode: crashAfterResponse}
			ctx := context.WithValue(context.Background(), recoveryCtxKey{}, true)
			client := stdlib.NewJournaledLLM(p.newClient(ts.URL), j)
			req := contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: "read"}}, Tools: []contract.ToolDef{{Name: "read"}}}
			if _, err := client.Chat(ctx, req); !errors.Is(err, errRecoveryCrash) {
				t.Fatalf("crash = %v", err)
			}
			j.cursor = 0
			recovered, err := client.Chat(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if physical.Load() != 1 {
				t.Fatal("replay made a duplicate physical model request")
			}
			if len(recovered.Reasoning) != 1 || recovered.Reasoning[0].Signature != "opaque-native" {
				t.Fatalf("reasoning lost: %+v", recovered.Reasoning)
			}
			if u := recovered.Usage; u.InputTokens != 20 || u.CacheReadTokens != 0 || !u.CacheReadReported {
				t.Fatalf("accounting = %+v", u)
			}
			if len(recovered.ToolCalls) != 2 || recovered.ToolCalls[0].ID != "a" || recovered.ToolCalls[1].ID != "b" {
				t.Fatalf("tool order = %+v", recovered.ToolCalls)
			}
			req.Messages = append(req.Messages, recovered.AsMessage(), contract.Message{Role: "tool", ToolCallID: "a", Content: "receipt-a", Parts: []contract.ContentPart{{Type: "text", Text: "receipt-a"}, {Type: "image", MediaType: "image/png", Data: "AAAA"}}}, contract.Message{Role: "tool", ToolCallID: "b", Content: "receipt-b"})
			final, err := client.Chat(ctx, req)
			if err != nil || final.Content != "done" || physical.Load() != 2 {
				t.Fatalf("continuation = %+v, %v", final, err)
			}
			if final.Usage.CacheReadReported || final.Usage.CacheWriteReported {
				t.Fatalf("missing subtotals invented: %+v", final.Usage)
			}
		})
	}
}

func TestNativeProviderRequestConstraints(t *testing.T) {
	for _, p := range nativeProtocols() {
		t.Run(p.name, func(t *testing.T) {
			var physical atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				physical.Add(1)
				var body map[string]json.RawMessage
				json.NewDecoder(r.Body).Decode(&body)
				key := "text"
				if p.name == "anthropic" {
					key = "output_config"
				}
				if !strings.Contains(string(body[key]), `"answer"`) {
					t.Errorf("output schema not on the wire: %s", body[key])
				}
				io.WriteString(w, p.last)
			}))
			defer ts.Close()
			client := p.newClient(ts.URL)
			base := contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: "x"}}, Tools: []contract.ToolDef{{Name: "read"}}}
			for _, choice := range []*contract.ToolChoice{{Mode: "invalid"}, {Mode: contract.ToolChoiceTool, Name: "missing"}, {Mode: contract.ToolChoiceRequired, Name: "read"}} {
				req := base
				req.ToolChoice = choice
				if _, err := client.Chat(context.Background(), req); err == nil {
					t.Fatalf("invalid choice accepted: %+v", choice)
				}
			}
			req := base
			req.Tools = nil
			req.ToolChoice = &contract.ToolChoice{Mode: contract.ToolChoiceRequired}
			if _, err := client.Chat(context.Background(), req); err == nil {
				t.Fatal("forced tool choice without tools accepted")
			}
			bad := json.RawMessage(`null`)
			req = base
			req.Schema = &bad
			if _, err := client.Chat(context.Background(), req); err == nil {
				t.Fatal("invalid output schema accepted")
			}
			if physical.Load() != 0 {
				t.Fatal("invalid requests reached the provider")
			}
			schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`)
			req = base
			req.Schema = &schema
			if _, err := client.Chat(context.Background(), req); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNativeProviderTruncationAndCancellation(t *testing.T) {
	for _, p := range nativeProtocols() {
		t.Run(p.name, func(t *testing.T) {
			for _, stream := range []string{"data: {invalid}\n\n" + p.last, "data: {\"type\":\"ping\"}\n\n"} {
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, stream) }))
				_, err := p.newClient(ts.URL).Chat(context.Background(), contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: "x"}}})
				ts.Close()
				if err == nil {
					t.Fatal("corrupt or truncated stream was accepted")
				}
			}
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer ts.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := p.newClient(ts.URL).Chat(ctx, contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: "x"}}})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(fmt.Sprintf("cancellation lost: %v", err))
			}
		})
	}
}
