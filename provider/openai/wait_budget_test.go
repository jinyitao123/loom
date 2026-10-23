package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
)

// recordedStream serves an SSE response shaped by script and reports when the
// server saw the request go away.
func recordedStream(t *testing.T, script func(w http.ResponseWriter, r *http.Request)) (*Client, <-chan struct{}) {
	t.Helper()
	gone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		script(w, r)
		<-r.Context().Done()
		close(gone)
	}))
	t.Cleanup(server.Close)
	return New("test-key", WithBaseURL(server.URL)), gone
}

func startStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
}

func writeContent(w http.ResponseWriter, text string) {
	_, _ = fmt.Fprintf(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", text)
	w.(http.Flusher).Flush()
}

// drain reads the stream to its end and returns its error and the content.
func drain(t *testing.T, stream <-chan contract.StreamChunk) (string, error) {
	t.Helper()
	var content string
	timeout := time.After(5 * time.Second)
	for {
		select {
		case chunk, open := <-stream:
			if !open {
				return content, nil
			}
			if chunk.Err != nil {
				return content, chunk.Err
			}
			content += chunk.Content
		case <-timeout:
			t.Fatal("stream did not end")
		}
	}
}

func TestWaitBudgetStages(t *testing.T) {
	budget := &contract.WaitBudget{FirstChunkMS: 150, IdleMS: 150}
	cases := []struct {
		name   string
		script func(w http.ResponseWriter, r *http.Request)
		stage  string
	}{
		{name: "late first chunk", stage: "first_chunk", script: func(w http.ResponseWriter, r *http.Request) {
			startStream(w)
		}},
		{name: "heartbeats without model data", stage: "first_chunk", script: func(w http.ResponseWriter, r *http.Request) {
			startStream(w)
			for range 20 {
				_, _ = fmt.Fprint(w, ": ping\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(30 * time.Millisecond):
				}
			}
		}},
		{name: "stall mid-stream", stage: "stream_idle", script: func(w http.ResponseWriter, r *http.Request) {
			startStream(w)
			writeContent(w, "partial")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, gone := recordedStream(t, tc.script)
			started := time.Now()
			stream, err := client.Stream(context.Background(), contract.ChatRequest{Model: "m", WaitBudget: budget,
				Messages: []contract.Message{{Role: "user", Content: "go"}}})
			if err == nil {
				_, err = drain(t, stream)
			}
			var stage *StageTimeoutError
			if !errors.As(err, &stage) || stage.Stage != tc.stage || !errors.Is(err, ErrStageTimeout) {
				t.Fatalf("error = %v, want %s stage timeout", err, tc.stage)
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("budget took %s to end the call", elapsed)
			}
			select {
			case <-gone:
			case <-time.After(2 * time.Second):
				t.Fatal("the request was not cancelled upstream")
			}
		})
	}
}

func TestWaitBudgetLetsSteadyGenerationFinish(t *testing.T) {
	client, _ := recordedStream(t, func(w http.ResponseWriter, r *http.Request) {
		startStream(w)
		for i := range 8 {
			writeContent(w, fmt.Sprint(i))
			time.Sleep(60 * time.Millisecond)
		}
		_, _ = fmt.Fprint(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		w.(http.Flusher).Flush()
	})
	// The whole generation outlasts the first-chunk budget; only the gaps count.
	stream, err := client.Stream(context.Background(), contract.ChatRequest{Model: "m",
		WaitBudget: &contract.WaitBudget{FirstChunkMS: 200, IdleMS: 200}, Messages: []contract.Message{{Role: "user", Content: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if content, err := drain(t, stream); err != nil || content != "01234567" {
		t.Fatalf("steady generation ended with %q, %v", content, err)
	}
}

func TestWaitBudgetDefersToCallerCancellationAndLateHeaders(t *testing.T) {
	client, _ := recordedStream(t, func(w http.ResponseWriter, r *http.Request) { startStream(w) })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	stream, err := client.Stream(ctx, contract.ChatRequest{Model: "m",
		WaitBudget: &contract.WaitBudget{FirstChunkMS: 2000}, Messages: []contract.Message{{Role: "user", Content: "go"}}})
	if err == nil {
		_, err = drain(t, stream)
	}
	if !errors.Is(err, ErrStreamCancelled) || errors.Is(err, ErrStageTimeout) {
		t.Fatalf("caller cancellation reported as %v", err)
	}

	// Headers that never arrive end the call at the first-chunk budget too.
	slow, _ := recordedStream(t, func(w http.ResponseWriter, r *http.Request) {})
	_, err = slow.Stream(context.Background(), contract.ChatRequest{Model: "m",
		WaitBudget: &contract.WaitBudget{FirstChunkMS: 150}, Messages: []contract.Message{{Role: "user", Content: "go"}}})
	var stage *StageTimeoutError
	if !errors.As(err, &stage) || stage.Stage != "first_chunk" {
		t.Fatalf("late headers ended with %v", err)
	}
}

// A total budget ends a call that keeps producing output past it, which no
// stage budget would end, and leaves a call that finishes in time alone.
func TestWaitBudgetTotalBoundsSteadyGeneration(t *testing.T) {
	steady := func(w http.ResponseWriter, r *http.Request) {
		startStream(w)
		for i := range 10 {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			writeContent(w, fmt.Sprint(i))
			time.Sleep(50 * time.Millisecond)
		}
		_, _ = fmt.Fprint(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}
	client, gone := recordedStream(t, steady)
	stream, err := client.Stream(context.Background(), contract.ChatRequest{Model: "m",
		WaitBudget: &contract.WaitBudget{FirstChunkMS: 200, IdleMS: 200, TotalMS: 220}, Messages: []contract.Message{{Role: "user", Content: "go"}}})
	if err == nil {
		_, err = drain(t, stream)
	}
	var stage *StageTimeoutError
	if !errors.As(err, &stage) || stage.Stage != "total" || stage.Budget != 220*time.Millisecond {
		t.Fatalf("error = %v, want total stage timeout", err)
	}
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatal("the request was not cancelled upstream")
	}

	client, _ = recordedStream(t, steady)
	stream, err = client.Stream(context.Background(), contract.ChatRequest{Model: "m",
		WaitBudget: &contract.WaitBudget{TotalMS: 3000}, Messages: []contract.Message{{Role: "user", Content: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if content, err := drain(t, stream); err != nil || content != "0123456789" {
		t.Fatalf("call within its total budget ended with %q, %v", content, err)
	}
}
