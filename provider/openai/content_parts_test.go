package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

// captureMessages returns a client whose requests are decoded into *got.
func captureMessages(t *testing.T, got *[]map[string]any) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		*got = body.Messages
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	return New("k", WithBaseURL(srv.URL)), srv.Close
}

func TestUserImagePartsBecomeContentArray(t *testing.T) {
	var got []map[string]any
	c, done := captureMessages(t, &got)
	defer done()

	_, err := c.Chat(context.Background(), contract.ChatRequest{Model: "m", Messages: []contract.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "what is this?", Parts: []contract.ContentPart{
			{Type: "text", Text: "what is this?"},
			{Type: "image", MediaType: "image/png", Data: "iVBORw0K"},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0]["content"] != "sys" {
		t.Fatalf("system content = %v", got[0]["content"])
	}
	parts, ok := got[1]["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("user content = %#v", got[1]["content"])
	}
	image := parts[1].(map[string]any)
	if image["type"] != "image_url" || image["image_url"].(map[string]any)["url"] != "data:image/png;base64,iVBORw0K" {
		t.Fatalf("image part = %#v", image)
	}
}

func TestToolImagesFollowTheToolBatch(t *testing.T) {
	var got []map[string]any
	c, done := captureMessages(t, &got)
	defer done()

	_, err := c.Chat(context.Background(), contract.ChatRequest{Model: "m", Messages: []contract.Message{
		{Role: "user", Content: "look at both"},
		{Role: "assistant", ToolCalls: []contract.ToolCall{{ID: "a", Name: "read", Args: "{}"}, {ID: "b", Name: "read", Args: "{}"}}},
		{Role: "tool", ToolCallID: "a", Content: "Read image a.png", Parts: []contract.ContentPart{{Type: "text", Text: "Read image a.png"}, {Type: "image", MediaType: "image/png", Data: "AAAA"}}},
		{Role: "tool", ToolCallID: "b", Content: "plain text"},
		{Role: "user", Content: "thanks"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range got {
		roles = append(roles, m["role"].(string))
	}
	want := []string{"user", "assistant", "tool", "tool", "user", "user"}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles = %v, want %v", roles, want)
		}
	}
	if got[2]["content"] != "Read image a.png" {
		t.Fatalf("tool message keeps its text: %#v", got[2]["content"])
	}
	images := got[4]["content"].([]any)
	if len(images) != 2 || images[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("injected image message = %#v", got[4]["content"])
	}
	if got[5]["content"] != "thanks" {
		t.Fatalf("following user message = %#v", got[5])
	}
}

func TestTextOnlyMessagesAreUnchanged(t *testing.T) {
	c := New("k")
	msgs := c.buildMessages([]contract.Message{
		{Role: "user", Content: "hi", Parts: []contract.ContentPart{{Type: "text", Text: "hi"}}},
		{Role: "assistant", ToolCalls: []contract.ToolCall{{ID: "x", Name: "t", Args: "{}"}}},
	})
	raw, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"index":0,"id":"x","type":"function","function":{"name":"t","arguments":"{}"}}]}]`
	if string(raw) != want {
		t.Fatalf("wire messages:\n got %s\nwant %s", raw, want)
	}
}
