package openai_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/openai"
)

func TestReasoningContentReplayIsOptInAndPreservesToolHistory(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, streaming := range []bool{false, true} {
			t.Run(map[bool]string{false: "default", true: "opt_in"}[enabled]+map[bool]string{false: "/chat", true: "/stream"}[streaming], func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body struct {
						Messages []map[string]any `json:"messages"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					assistant := body.Messages[1]
					value, present := assistant["reasoning_content"]
					if enabled && value != "synthetic continuity" || !enabled && present {
						t.Errorf("reasoning dialect: enabled=%v value=%v present=%v", enabled, value, present)
					}
					if body.Messages[2]["tool_call_id"] != "call-1" || assistant["tool_calls"] == nil {
						t.Error("tool history changed")
					}
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
					}
				}))
				defer server.Close()
				opts := []openai.Option{openai.WithBaseURL(server.URL)}
				if enabled {
					opts = append(opts, openai.WithReasoningContentReplay())
				}
				client := openai.New("synthetic", opts...)
				request := contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: "synthetic request"}, {Role: "assistant", Reasoning: []contract.Reasoning{{Text: "synthetic continuity"}}, ToolCalls: []contract.ToolCall{{ID: "call-1", Name: "lookup", Args: `{}`}}}, {Role: "tool", ToolCallID: "call-1", Content: "result"}}}
				if streaming {
					stream, err := client.Stream(t.Context(), request)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range stream {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := client.Chat(t.Context(), request); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestReasoningContentRejectsUnrepresentableHistoryBeforeTransport(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	for _, block := range []contract.Reasoning{{Text: "opaque", Signature: "signature"}, {Redacted: true}} {
		client := openai.New("synthetic", openai.WithBaseURL(server.URL), openai.WithReasoningContentReplay())
		request := contract.ChatRequest{Messages: []contract.Message{{Role: "assistant", Reasoning: []contract.Reasoning{block}}}}
		if _, err := client.Chat(t.Context(), request); err == nil || !strings.Contains(err.Error(), "cannot represent") {
			t.Fatal("unrepresentable block accepted")
		}
		if stream, err := client.Stream(t.Context(), request); err == nil || stream != nil || !strings.Contains(err.Error(), "cannot represent") {
			t.Fatal("unrepresentable stream accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unrepresentable data reached transport")
	}
}

func TestCachedUsagePresenceAndDeepSeekDialect(t *testing.T) {
	for _, stats := range []struct {
		body  string
		known bool
		read  int
	}{
		{`"prompt_tokens":100,"completion_tokens":7`, false, 0},
		{`"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":0}`, true, 0},
		{`"prompt_tokens":100,"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20`, true, 80},
	} {
		response, err := chatWithServer(t, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{`+stats.body+`}}`)
		if err != nil {
			t.Fatal(err)
		}
		if response.Usage.CacheReadTokens != stats.read || response.Usage.CacheReadReported != stats.known || response.Usage.CacheWriteReported || response.Usage.CacheWriteTokens != 0 || response.Usage.InputTokens != 100 {
			t.Fatalf("cache accounting = %+v", response.Usage)
		}
	}
}
