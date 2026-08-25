package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/openai"
)

// chatWithServer drives one non-streaming Chat call against a canned server.
func chatWithServer(t *testing.T, serverBody string, opts ...openai.Option) (*contract.ChatResponse, error) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, serverBody)
	}))
	t.Cleanup(ts.Close)
	client := openai.New("test-key", append([]openai.Option{openai.WithBaseURL(ts.URL)}, opts...)...)
	return client.Chat(context.Background(), chatReq(false, 0.7))
}

// collectStream drains a chunk channel until close, with a safety timeout.
func collectStream(t *testing.T, ch <-chan contract.StreamChunk) []contract.StreamChunk {
	t.Helper()
	var out []contract.StreamChunk
	timeout := time.After(5 * time.Second)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, chunk)
		case <-timeout:
			t.Fatal("stream did not close within 5s")
		}
	}
}

// streamWithServer starts an SSE server writing raw wire and returns the
// collected chunks for one Stream call.
func streamWithServer(t *testing.T, wire string, opts ...openai.Option) []contract.StreamChunk {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, wire)
	}))
	t.Cleanup(ts.Close)
	client := openai.New("test-key", append([]openai.Option{openai.WithBaseURL(ts.URL)}, opts...)...)
	ch, err := client.Stream(context.Background(), chatReq(false, 0.7))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return collectStream(t, ch)
}

func firstErr(t *testing.T, chunks []contract.StreamChunk) error {
	t.Helper()
	for _, c := range chunks {
		if c.Err != nil {
			return c.Err
		}
	}
	return nil
}

// TestChatEmptyContentFailsClosed covers HTTP 200 with empty content and no
// tool calls: the call must fail with ErrEmptyResponse carrying finish_reason,
// never return a silent empty success.
func TestChatEmptyContentFailsClosed(t *testing.T) {
	resp, err := chatWithServer(t, `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`)
	if err == nil {
		t.Fatalf("expected error, got response %+v", resp)
	}
	if !errors.Is(err, openai.ErrEmptyResponse) {
		t.Fatalf("errors.Is(err, ErrEmptyResponse) = false, err = %v", err)
	}
	var ee *openai.EmptyResponseError
	if !errors.As(err, &ee) {
		t.Fatalf("error %v is not *EmptyResponseError", err)
	}
	if ee.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q, want stop", ee.FinishReason)
	}
	if ee.Stream {
		t.Fatal("non-stream empty response must not set Stream=true")
	}
}

// TestChatNullContentFailsClosed covers wire "content":null, which unmarshals
// to "" and must take the same fail-closed path.
func TestChatNullContentFailsClosed(t *testing.T) {
	_, err := chatWithServer(t, `{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"length"}]}`)
	if err == nil {
		t.Fatal("expected error for null content")
	}
	if !errors.Is(err, openai.ErrEmptyResponse) {
		t.Fatalf("errors.Is(err, ErrEmptyResponse) = false, err = %v", err)
	}
	var ee *openai.EmptyResponseError
	if !errors.As(err, &ee) || ee.FinishReason != "length" {
		t.Fatalf("expected FinishReason=length in %#v", err)
	}
}

// TestChatToolCallsEmptyContentIsLegal pins the legal exception: tool_calls
// non-empty with empty content is a normal response, never an empty-response
// failure.
func TestChatToolCallsEmptyContentIsLegal(t *testing.T) {
	resp, err := chatWithServer(t, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]},"finish_reason":"tool_calls"}]}`)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want 1", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Name != "lookup" || resp.ToolCalls[0].Args != `{"q":1}` {
		t.Fatalf("unexpected tool call: %+v", resp.ToolCalls[0])
	}
	if resp.StopReason != "tool_calls" {
		t.Fatalf("StopReason = %q, want tool_calls", resp.StopReason)
	}
}

// TestChatFinishReasonPropagation pins non-stream finish_reason propagation.
func TestChatFinishReasonPropagation(t *testing.T) {
	for _, fr := range []string{"stop", "length", "tool_calls"} {
		t.Run(fr, func(t *testing.T) {
			resp, err := chatWithServer(t,
				`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"`+fr+`"}]}`)
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if resp.StopReason != fr {
				t.Fatalf("StopReason = %q, want %q", resp.StopReason, fr)
			}
			if resp.Content != "ok" {
				t.Fatalf("Content = %q, want ok", resp.Content)
			}
		})
	}
}

// TestStreamEmptyDoneFailsClosed covers "[DONE] with no content and no tool
// calls": the error must travel through the stream error channel.
func TestStreamEmptyDoneFailsClosed(t *testing.T) {
	chunks := streamWithServer(t,
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: [DONE]\n\n")
	err := firstErr(t, chunks)
	if err == nil {
		t.Fatal("expected stream error for empty [DONE]")
	}
	if !errors.Is(err, openai.ErrEmptyResponse) {
		t.Fatalf("errors.Is(err, ErrEmptyResponse) = false, err = %v", err)
	}
	var ee *openai.EmptyResponseError
	if !errors.As(err, &ee) {
		t.Fatalf("error %v is not *EmptyResponseError", err)
	}
	if !ee.Stream || ee.CloseKind != "done" || ee.FinishReason != "stop" {
		t.Fatalf("unexpected empty-response context: %+v", ee)
	}
	for _, c := range chunks {
		if c.Done {
			t.Fatal("error chunk must not carry Done=true (would look like clean completion)")
		}
	}
}

// TestStreamEOFWithoutDoneErrors covers a clean EOF without [DONE]: the
// consumer must receive ErrStreamClosedWithoutDone, never a clean close.
func TestStreamEOFWithoutDoneErrors(t *testing.T) {
	chunks := streamWithServer(t, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
	if len(chunks) != 2 || chunks[0].Content != "partial" {
		t.Fatalf("chunks = %+v, want content chunk then error chunk", chunks)
	}
	err := firstErr(t, chunks)
	if err == nil {
		t.Fatal("expected error for EOF without [DONE]")
	}
	if !errors.Is(err, openai.ErrStreamClosedWithoutDone) {
		t.Fatalf("errors.Is(err, ErrStreamClosedWithoutDone) = false, err = %v", err)
	}
}

// TestStreamMalformedLineThenDone pins parser robustness: one malformed data
// line is skipped, the rest of the stream completes normally.
func TestStreamMalformedLineThenDone(t *testing.T) {
	chunks := streamWithServer(t,
		"data: {not-json\n\n"+
			"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n"+
			"data: [DONE]\n\n")
	if err := firstErr(t, chunks); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	got := ""
	sawDone := false
	for _, c := range chunks {
		got += c.Content
		if c.Done {
			sawDone = true
		}
	}
	if got != "ok" || !sawDone {
		t.Fatalf("got=%q sawDone=%v, want ok/true", got, sawDone)
	}
}

// TestStreamDataNoSpace accepts "data:{...}" with zero spaces after the colon.
func TestStreamDataNoSpace(t *testing.T) {
	chunks := streamWithServer(t,
		"data:{\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n"+
			"data:[DONE]\n\n")
	if err := firstErr(t, chunks); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	got := ""
	sawDone := false
	for _, c := range chunks {
		got += c.Content
		if c.Done {
			sawDone = true
		}
	}
	if got != "ok" || !sawDone {
		t.Fatalf("got=%q sawDone=%v, want ok/true", got, sawDone)
	}
}

// TestStreamScannerTooLongLine reports an over-long line as a scanner error
// instead of silently truncating or closing cleanly.
func TestStreamScannerTooLongLine(t *testing.T) {
	longLine := `data: {"choices":[{"delta":{"content":"` + strings.Repeat("x", 2*1024*1024) + `"},"finish_reason":null}]}`
	chunks := streamWithServer(t, longLine+"\n\n")
	err := firstErr(t, chunks)
	if err == nil {
		t.Fatal("expected scanner error for over-long line")
	}
	if !errors.Is(err, openai.ErrStreamScanner) {
		t.Fatalf("errors.Is(err, ErrStreamScanner) = false, err = %v", err)
	}
}

// TestStreamFinishReasonPropagation pins finish_reason on the terminal chunk.
func TestStreamFinishReasonPropagation(t *testing.T) {
	for _, fr := range []string{"stop", "length", "tool_calls"} {
		t.Run(fr, func(t *testing.T) {
			chunks := streamWithServer(t,
				"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n"+
					"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\""+fr+"\"}]}\n\n"+
					"data: [DONE]\n\n")
			var terminal *contract.StreamChunk
			for i := range chunks {
				if chunks[i].Done {
					terminal = &chunks[i]
				}
			}
			if terminal == nil {
				t.Fatalf("no terminal chunk in %+v", chunks)
			}
			if terminal.FinishReason != fr {
				t.Fatalf("terminal FinishReason = %q, want %q", terminal.FinishReason, fr)
			}
		})
	}
}

// TestAttemptTimeoutChat covers a server slower than the attempt timeout: the
// client fails with a timeout-classified error (retryable by llmrouter).
func TestAttemptTimeoutChat(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Respond slower than the client's attempt timeout (50ms).
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"late"}}]}`)
	}))
	t.Cleanup(ts.Close)
	client := openai.New("test-key",
		openai.WithBaseURL(ts.URL),
		openai.WithAttemptTimeout(50*time.Millisecond),
	)
	_, err := client.Chat(context.Background(), chatReq(false, 0.7))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "deadline exceeded") && !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("error %q is not timeout-classified", err)
	}
}

// TestAttemptTimeoutStream covers the streaming body read exceeding the
// attempt timeout: the stream must end with a scanner-classified error that
// still carries the retryable deadline marker.
func TestAttemptTimeoutStream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
		flusher.Flush()
		time.Sleep(500 * time.Millisecond) // never sends [DONE] within the timeout
	}))
	t.Cleanup(ts.Close)
	client := openai.New("test-key",
		openai.WithBaseURL(ts.URL),
		openai.WithAttemptTimeout(100*time.Millisecond),
	)
	ch, err := client.Stream(context.Background(), chatReq(false, 0.7))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := collectStream(t, ch)
	err = firstErr(t, chunks)
	if err == nil {
		t.Fatal("expected stream timeout error")
	}
	if !errors.Is(err, openai.ErrStreamScanner) && !errors.Is(err, openai.ErrStreamCancelled) {
		t.Fatalf("errors.Is scanner/cancel = false, err = %v", err)
	}
	if !strings.Contains(err.Error(), "deadline exceeded") && !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("stream timeout error %q lost the retryable deadline marker", err)
	}
}

// TestJSONModeInstructionRequired covers the pre-send guard: json_object mode
// with a Schema but no explicit JSON instruction must fail before the request
// leaves the client (zero server hits); with an instruction the request goes
// out normally. Case-insensitive matching covers Chinese prompts embedding the
// ASCII term.
func TestJSONModeInstructionRequired(t *testing.T) {
	tests := []struct {
		name     string
		messages []contract.Message
		wantErr  bool
	}{
		{name: "missing-instruction", messages: []contract.Message{{Role: "system", Content: "回答用户问题。"}}, wantErr: true},
		{name: "chinese-json", messages: []contract.Message{{Role: "system", Content: "请输出JSON格式的结果。"}}, wantErr: false},
		{name: "lowercase-json", messages: []contract.Message{{Role: "user", Content: "return json"}}, wantErr: false},
		{name: "mixed-case-json", messages: []contract.Message{{Role: "user", Content: "Give me Json output"}}, wantErr: false},
	}
	for _, stream := range []bool{false, true} {
		for _, tt := range tests {
			name := "chat"
			if stream {
				name = "stream"
			}
			t.Run(name+"-"+tt.name, func(t *testing.T) {
				var hits int32
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					atomic.AddInt32(&hits, 1)
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
				}))
				t.Cleanup(ts.Close)
				client := openai.New("test-key",
					openai.WithBaseURL(ts.URL),
					openai.WithJSONObjectMode(),
				)
				schema := json.RawMessage(`{"type":"object"}`)
				req := contract.ChatRequest{
					Model:    "test-model",
					Messages: tt.messages,
					Schema:   &schema,
				}
				var err error
				if stream {
					ch, streamErr := client.Stream(context.Background(), req)
					if streamErr == nil {
						collectStream(t, ch)
					}
					err = streamErr
				} else {
					_, err = client.Chat(context.Background(), req)
				}
				if tt.wantErr {
					if err == nil {
						t.Fatal("expected ErrJSONModeInstructionMissing, got nil")
					}
					if !errors.Is(err, openai.ErrJSONModeInstructionMissing) {
						t.Fatalf("errors.Is = false, err = %v", err)
					}
					if got := atomic.LoadInt32(&hits); got != 0 {
						t.Fatalf("request reached server despite pre-send failure: hits=%d", got)
					}
				} else {
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
					if got := atomic.LoadInt32(&hits); got != 1 {
						t.Fatalf("hits = %d, want 1", got)
					}
				}
			})
		}
	}
}

// TestJSONModeWithoutSchemaUnchanged pins zero regression: json_object mode
// without a Schema must not trigger the pre-send guard (no prompt change for
// non-structured requests).
func TestJSONModeWithoutSchemaUnchanged(t *testing.T) {
	var mu sync.Mutex
	var body []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = b
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(ts.Close)
	client := openai.New("test-key", openai.WithBaseURL(ts.URL), openai.WithJSONObjectMode())
	resp, err := client.Chat(context.Background(), contract.ChatRequest{
		Model:    "test-model",
		Messages: []contract.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("Content = %q, want ok", resp.Content)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(string(body), "response_format") {
		t.Fatalf("unexpected response_format in body: %s", body)
	}
}
