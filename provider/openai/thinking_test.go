package openai_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/openai"
)

// newWireServer starts an httptest server that captures the request body and
// answers with either a chat completion (stream=false) or an SSE stream
// (stream=true). The returned func returns a copy of the captured body.
func newWireServer(t *testing.T, stream bool) (*httptest.Server, func() []byte) {
	t.Helper()
	var mu sync.Mutex
	var captured []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		captured = body
		mu.Unlock()
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(ts.Close)
	return ts, func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return append([]byte(nil), captured...)
	}
}

// chatReq builds a contract request with an explicit temperature and optional
// tools; temperature is intentionally set even for thinking-enabled cases to
// prove the client strips it rather than relying on the caller.
func chatReq(withTools bool, temperature float64) contract.ChatRequest {
	req := contract.ChatRequest{
		Model:       "test-model",
		Messages:    []contract.Message{{Role: "user", Content: "hi"}},
		Temperature: &temperature,
	}
	if withTools {
		req.Tools = []contract.ToolDef{{
			Name:        "lookup",
			Description: "look things up",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}}
	}
	return req
}

// thinkingType decodes the captured body and returns the thinking.type value.
// ok=false means the thinking key is absent from the wire.
func thinkingType(t *testing.T, body []byte) (string, bool) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal captured body: %v\nbody=%s", err, body)
	}
	raw, ok := m["thinking"]
	if !ok {
		return "", false
	}
	tm, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("thinking = %#v, want object", raw)
	}
	typ, _ := tm["type"].(string)
	return typ, true
}

func hasWireField(t *testing.T, body []byte, field string) bool {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal captured body: %v", err)
	}
	_, ok := m[field]
	return ok
}

func runChat(t *testing.T, client contract.LLM, req contract.ChatRequest) {
	t.Helper()
	resp, err := client.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q, want ok", resp.Content)
	}
}

func runStream(t *testing.T, client contract.LLM, req contract.ChatRequest) {
	t.Helper()
	ch, err := client.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				t.Fatal("stream closed without Done")
			}
			if chunk.Done {
				return
			}
		case <-timeout:
			t.Fatal("stream did not finish within 5s")
		}
	}
}

// TestThinkingOptInToolsDisabled covers: opt-in + tools → thinking disabled
// on both Chat and Stream wires; temperature stays (only enabled strips it).
func TestThinkingOptInToolsDisabled(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "chat"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			ts, capture := newWireServer(t, stream)
			client := openai.New("test-key",
				openai.WithBaseURL(ts.URL),
				openai.WithThinkingControl("enabled", true),
			)
			req := chatReq(true, 0.7)
			if stream {
				runStream(t, client, req)
			} else {
				runChat(t, client, req)
			}
			body := capture()
			typ, ok := thinkingType(t, body)
			if !ok {
				t.Fatal("thinking key missing on opt-in request with tools")
			}
			if typ != "disabled" {
				t.Fatalf("thinking.type = %q, want disabled", typ)
			}
			if !hasWireField(t, body, "temperature") {
				t.Fatal("temperature must stay when thinking is disabled")
			}
		})
	}
}

// TestThinkingOptInNoToolsEnabled covers: opt-in + no tools + defaultMode
// enabled → thinking enabled and temperature stripped even when the request
// explicitly set it, on both Chat and Stream wires.
func TestThinkingOptInNoToolsEnabled(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "chat"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			ts, capture := newWireServer(t, stream)
			client := openai.New("test-key",
				openai.WithBaseURL(ts.URL),
				openai.WithThinkingControl("enabled", true),
			)
			req := chatReq(false, 0.7)
			if stream {
				runStream(t, client, req)
			} else {
				runChat(t, client, req)
			}
			body := capture()
			typ, ok := thinkingType(t, body)
			if !ok {
				t.Fatal("thinking key missing on opt-in request without tools")
			}
			if typ != "enabled" {
				t.Fatalf("thinking.type = %q, want enabled", typ)
			}
			if hasWireField(t, body, "temperature") {
				t.Fatal("temperature must be stripped while thinking is enabled")
			}
		})
	}
}

// TestThinkingDefaultDisabledAlways covers: defaultMode=disabled → thinking is
// disabled with and without tools, on both Chat and Stream wires.
func TestThinkingDefaultDisabledAlways(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, withTools := range []bool{false, true} {
			name := "chat"
			if stream {
				name = "stream"
			}
			tools := "no-tools"
			if withTools {
				tools = "with-tools"
			}
			t.Run(name+"-"+tools, func(t *testing.T) {
				ts, capture := newWireServer(t, stream)
				client := openai.New("test-key",
					openai.WithBaseURL(ts.URL),
					openai.WithThinkingControl("disabled", true),
				)
				req := chatReq(withTools, 0.3)
				if stream {
					runStream(t, client, req)
				} else {
					runChat(t, client, req)
				}
				typ, ok := thinkingType(t, capture())
				if !ok {
					t.Fatal("thinking key missing on opt-in request")
				}
				if typ != "disabled" {
					t.Fatalf("thinking.type = %q, want disabled", typ)
				}
			})
		}
	}
}

// TestNoOptInZeroChange pins the zero-regression contract with golden bodies:
// an un-opted provider (OpenAI/OneAPI/any compatible endpoint) sees the exact
// request the pre-change client produced — no thinking key, temperature sent.
func TestNoOptInZeroChange(t *testing.T) {
	golden := []struct {
		name      string
		withTools bool
		want      string
	}{
		{
			name:      "no-tools",
			withTools: false,
			want:      `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`,
		},
		{
			name:      "with-tools",
			withTools: true,
			want:      `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","description":"look things up","parameters":{"type":"object"}}}],"temperature":0.7}`,
		},
	}
	for _, stream := range []bool{false, true} {
		for _, tc := range golden {
			name := "chat"
			if stream {
				name = "stream"
			}
			t.Run(name+"-"+tc.name, func(t *testing.T) {
				ts, capture := newWireServer(t, stream)
				client := openai.New("test-key", openai.WithBaseURL(ts.URL))
				req := chatReq(tc.withTools, 0.7)
				if stream {
					runStream(t, client, req)
				} else {
					runChat(t, client, req)
				}
				body := capture()
				want := tc.want
				if stream {
					// Stream requests always carried these two pre-existing
					// fields before M1; they are part of the zero-change body.
					want = strings.Replace(want, `"temperature":0.7}`, `"temperature":0.7,"stream":true,"stream_options":{"include_usage":true}}`, 1)
				}
				if string(body) != want {
					t.Fatalf("un-opted request body changed\n got: %s\nwant: %s", body, want)
				}
				if bytes.Contains(body, []byte("thinking")) {
					t.Fatal("thinking key leaked into un-opted request")
				}
			})
		}
	}
}
