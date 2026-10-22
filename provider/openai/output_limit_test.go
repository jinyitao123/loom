package openai_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/openai"
)

// capturedRequests answers every chat call and keeps the raw JSON bodies.
func capturedRequests(t *testing.T) (*httptest.Server, chan map[string]any) {
	t.Helper()
	bodies := make(chan map[string]any, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies <- body
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, protocolFrame(map[string]any{"content": "ok"}, "stop")+"data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	return server, bodies
}

func TestOutputLimitFieldFollowsTheClientDeclaration(t *testing.T) {
	server, bodies := capturedRequests(t)
	request := contract.ChatRequest{Model: "m", Messages: []contract.Message{{Role: "user", Content: "hi"}}, MaxTokens: 321}
	for _, tc := range []struct {
		name          string
		opts          []openai.Option
		present, gone string
	}{
		{"default sends max_tokens", nil, "max_tokens", "max_completion_tokens"},
		{"declared sends max_completion_tokens", []openai.Option{openai.WithMaxCompletionTokens()}, "max_completion_tokens", "max_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := openai.New("k", append([]openai.Option{openai.WithBaseURL(server.URL)}, tc.opts...)...)
			if _, err := client.Chat(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			stream, err := client.Stream(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			collectStream(t, stream)
			for _, kind := range []string{"chat", "stream"} {
				body := <-bodies
				if body[tc.present] != float64(321) {
					t.Fatalf("%s: %s = %v in %v", kind, tc.present, body[tc.present], body)
				}
				if _, sent := body[tc.gone]; sent {
					t.Fatalf("%s: %s was also sent: %v", kind, tc.gone, body)
				}
			}
		})
	}

	// No limit means neither field is sent, whichever the client declares.
	client := openai.New("k", openai.WithBaseURL(server.URL), openai.WithMaxCompletionTokens())
	if _, err := client.Chat(context.Background(), contract.ChatRequest{Model: "m", Messages: request.Messages}); err != nil {
		t.Fatal(err)
	}
	stream, err := client.Stream(context.Background(), contract.ChatRequest{Model: "m", Messages: request.Messages})
	if err != nil {
		t.Fatal(err)
	}
	collectStream(t, stream)
	for range 2 {
		body := <-bodies
		if _, sent := body["max_tokens"]; sent {
			t.Fatalf("max_tokens sent without a limit: %v", body)
		}
		if _, sent := body["max_completion_tokens"]; sent {
			t.Fatalf("max_completion_tokens sent without a limit: %v", body)
		}
	}
}

func TestProtocolObservationNamesTheOutputLimitFieldAndTemperature(t *testing.T) {
	server, bodies := capturedRequests(t)
	temperature := 0.0
	for _, tc := range []struct {
		name        string
		opts        []openai.Option
		request     contract.ChatRequest
		limit       string
		temperature bool
	}{
		{"max_tokens", nil, contract.ChatRequest{MaxTokens: 10}, "max_tokens", false},
		{"max_completion_tokens with explicit zero temperature", []openai.Option{openai.WithMaxCompletionTokens()}, contract.ChatRequest{MaxTokens: 10, Temperature: &temperature}, "max_completion_tokens", true},
		{"nothing sent", nil, contract.ChatRequest{}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := make(chan contract.ProtocolObservation, 1)
			ctx := openai.WithProtocolObserver(context.Background(), openai.ProtocolObserverOptions{}, func(value contract.ProtocolObservation) { observed <- value })
			tc.request.Model, tc.request.Messages = "m", []contract.Message{{Role: "user", Content: "hi"}}
			client := openai.New("k", append([]openai.Option{openai.WithBaseURL(server.URL)}, tc.opts...)...)
			if _, err := client.Chat(ctx, tc.request); err != nil {
				t.Fatal(err)
			}
			<-bodies
			options := (<-observed).RequestOptions
			if options == nil || options.OutputLimit != tc.limit || options.Temperature != tc.temperature {
				t.Fatalf("request options = %+v", options)
			}
			stream, err := client.Stream(ctx, tc.request)
			if err != nil {
				t.Fatal(err)
			}
			collectStream(t, stream)
			<-bodies
			options = (<-observed).RequestOptions
			if options == nil || options.OutputLimit != tc.limit || options.Temperature != tc.temperature {
				t.Fatalf("stream request options = %+v", options)
			}
		})
	}
}
