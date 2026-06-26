package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

func envGetter(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestRunCmdIO_ChatInProcess(t *testing.T) {
	srv := httptest.NewServer((&mockLLM{turns: []llmTurn{{content: "in-proc answer"}}}).handler())
	defer srv.Close()

	orig, _ := os.Getwd()
	defer func() { _ = os.Chdir(orig) }()

	var out bytes.Buffer
	exit := runCmdIO(
		[]string{"--model", "test", "--cwd", t.TempDir()},
		strings.NewReader(`{"type":"chat","instruction":"hi"}`),
		&out,
		envGetter(map[string]string{"LOOM_BASE_URL": srv.URL, "LOOM_API_KEY": "x"}),
	)
	if exit != 0 {
		t.Fatalf("exit=%d out=%s", exit, out.String())
	}
	lines := parseLines(t, &out)
	if lines[0]["type"] != "session_init" || lines[len(lines)-1]["status"] != "completed" {
		t.Fatalf("unexpected events: %v", lines)
	}
}

func TestRunCmdIO_BadFlagReturns2(t *testing.T) {
	var out bytes.Buffer
	if exit := runCmdIO([]string{"--nope"}, strings.NewReader(""), &out, os.Getenv); exit != 2 {
		t.Fatalf("bad flag should exit 2, got %d", exit)
	}
}

func TestRunCmdIO_BadConfigStructuredFail(t *testing.T) {
	var out bytes.Buffer
	exit := runCmdIO([]string{"--model", "deepseek-chat"},
		strings.NewReader(`{"type":"chat","instruction":"hi"}`),
		&out, envGetter(nil))
	if exit != 1 {
		t.Fatalf("exit=%d", exit)
	}
	lines := parseLines(t, &out)
	if lines[len(lines)-1]["status"] != "failed" {
		t.Fatalf("want failed result: %v", lines)
	}
}

func TestRunCmdIO_EmptyStdinFails(t *testing.T) {
	var out bytes.Buffer
	if exit := runCmdIO([]string{"--model", "test"}, strings.NewReader(""), &out, os.Getenv); exit != 1 {
		t.Fatalf("empty stdin should fail, got %d", exit)
	}
}

func TestNewSinkSelectsFormat(t *testing.T) {
	var buf bytes.Buffer
	if _, ok := newSink("stream-json", &buf).(*Emitter); !ok {
		t.Fatal("stream-json should give *Emitter")
	}
	if _, ok := newSink("text", &buf).(textEmitter); !ok {
		t.Fatal("text should give textEmitter")
	}
}

func TestTextEmitterIsHumanReadable(t *testing.T) {
	var buf bytes.Buffer
	e := textEmitter{w: &buf}
	_ = e.SessionInit("s")
	_ = e.Thinking("hmm")
	_ = e.Text("hello")
	_ = e.ToolUse("c", "search", nil)
	_ = e.ToolResult("c", "done", "success")
	_ = e.TurnEnd("s")
	_ = e.Result("completed", "hello", "s")
	got := buf.String()
	if !strings.Contains(got, "hello") || !strings.Contains(got, "search") || !strings.Contains(got, "[completed]") {
		t.Fatalf("text output: %q", got)
	}
	if strings.Contains(got, `"type"`) {
		t.Fatalf("text mode must not emit JSON: %q", got)
	}
}

func TestTextEmitterError(t *testing.T) {
	var buf bytes.Buffer
	_ = textEmitter{w: &buf}.Error("boom")
	if !strings.Contains(buf.String(), "ERROR: boom") {
		t.Fatalf("text error: %q", buf.String())
	}
}

func TestNoToolsDispatchErrors(t *testing.T) {
	if _, err := (noTools{}).Dispatch(context.Background(), contract.ToolCall{Name: "x"}); err == nil {
		t.Fatal("noTools.Dispatch should error")
	}
}

func TestStreamingLLMStreamDelegates(t *testing.T) {
	inner := &streamLLM{chunks: []contract.StreamChunk{{Content: "x"}}}
	s := &streamingLLM{inner: inner, out: NewEmitter(&bytes.Buffer{})}
	ch, err := s.Stream(context.Background(), contract.ChatRequest{})
	if err != nil || ch == nil {
		t.Fatalf("Stream should delegate: ch=%v err=%v", ch, err)
	}
	for range ch { // drain so the producer goroutine finishes (no leak)
	}
}
