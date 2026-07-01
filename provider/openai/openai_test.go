package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

// fakeOAI serves a minimal valid chat-completions response and records the path
// it was called on, so we can assert URL construction offline.
func fakeOAI(t *testing.T, gotPath *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
}

func TestDefaultChatPath(t *testing.T) {
	var path string
	srv := fakeOAI(t, &path)
	defer srv.Close()

	c := New("k", WithBaseURL(srv.URL))
	if _, err := c.Chat(context.Background(), contract.ChatRequest{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if path != "/v1/chat/completions" {
		t.Fatalf("default path = %q, want /v1/chat/completions", path)
	}
}

func TestWithChatPath(t *testing.T) {
	var path string
	srv := fakeOAI(t, &path)
	defer srv.Close()

	// Mimic Zhipu GLM: base URL carries the version, chat path is bare.
	c := New("k", WithBaseURL(srv.URL+"/api/paas/v4"), WithChatPath("/chat/completions"))
	if _, err := c.Chat(context.Background(), contract.ChatRequest{Model: "glm-4-flash"}); err != nil {
		t.Fatal(err)
	}
	if path != "/api/paas/v4/chat/completions" {
		t.Fatalf("GLM path = %q, want /api/paas/v4/chat/completions", path)
	}
}
