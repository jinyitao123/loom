package deepseek

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
)

func streamWire(t *testing.T, wire string) []contract.StreamChunk {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
	}))
	t.Cleanup(server.Close)
	client := &Client{apiKey: "test-key", baseURL: server.URL, client: server.Client()}
	stream, err := client.Stream(context.Background(), contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var chunks []contract.StreamChunk
	timeout := time.After(5 * time.Second)
	for {
		select {
		case chunk, open := <-stream:
			if !open {
				return chunks
			}
			chunks = append(chunks, chunk)
		case <-timeout:
			t.Fatal("stream did not close")
		}
	}
}

func TestStreamKeepsSparseToolCallIndexes(t *testing.T) {
	wire := `data: {"choices":[{"delta":{"tool_calls":[{"index":3,"id":"b","type":"function","function":{"name":"second","arguments":"{}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"a","type":"function","function":{"name":"first","arguments":"{}"}}]}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	chunks := streamWire(t, wire)
	last := chunks[len(chunks)-1]
	if !last.Done || last.Err != nil || len(last.ToolCalls) != 2 || last.ToolCalls[0].ID != "a" || last.ToolCalls[1].ID != "b" {
		t.Fatalf("final chunk = %#v", last)
	}
}

func TestStreamWithoutDoneEndsWithError(t *testing.T) {
	wire := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"first","arguments":"{\"q\":"}}]}}]}` + "\n\n"
	chunks := streamWire(t, wire)
	last := chunks[len(chunks)-1]
	if last.Done || !errors.Is(last.Err, ErrStreamClosedWithoutDone) || len(last.ToolCalls) != 0 {
		t.Fatalf("truncated stream ended as %#v", last)
	}
}
