package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"go.uber.org/goleak"
)

// loomBin is the freshly-built `loom` binary, exercised by the E2E tests as a
// real subprocess (the only way to cover runCmd / the text sink / the streaming
// passthrough — i.e. the actual CLI path, not the fakes the unit tests use).
var loomBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "loom-e2e-bin")
	if err != nil {
		panic(err)
	}
	loomBin = filepath.Join(dir, "loom")
	build := exec.Command("go", "build", "-o", loomBin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic("e2e: failed to build loom binary: " + err.Error())
	}
	// goleak: fail the suite if any test leaks a goroutine (e.g. the streaming
	// goroutine not drained on error). Ignore the http transport's keep-alive
	// background goroutines (benign, framework-owned).
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
	)
}

// --- scriptable mock OpenAI-compatible server (real SSE streaming) ---

type scriptedToolCall struct{ id, name, args string }

type llmTurn struct {
	content   string
	toolCalls []scriptedToolCall
	httpError int // if >0, return this status (error path)
}

type mockLLM struct {
	mu       sync.Mutex
	turns    []llmTurn
	calls    int
	requests []string // raw request bodies, for assertions
}

func (m *mockLLM) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.requests = append(m.requests, string(body))
		i := m.calls
		m.calls++
		var turn llmTurn
		if i < len(m.turns) {
			turn = m.turns[i]
		}
		m.mu.Unlock()

		if turn.httpError > 0 {
			http.Error(w, `{"error":{"message":"upstream boom"}}`, turn.httpError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		writeData := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
			if fl != nil {
				fl.Flush()
			}
		}
		delta := func(d any) any {
			return map[string]any{"choices": []any{map[string]any{"delta": d}}}
		}

		// Stream the content as ≥2 deltas to exercise the streaming path.
		for _, part := range splitForStreaming(turn.content) {
			writeData(delta(map[string]any{"content": part}))
		}
		for _, tc := range turn.toolCalls {
			writeData(delta(map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "id": tc.id, "function": map[string]any{"name": tc.name, "arguments": tc.args}},
			}}))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
}

func splitForStreaming(s string) []string {
	if s == "" {
		return nil
	}
	if i := strings.IndexByte(s, ' '); i > 0 {
		return []string{s[:i], s[i:]}
	}
	if len(s) > 1 {
		return []string{s[:len(s)/2], s[len(s)/2:]}
	}
	return []string{s}
}

// --- E2E harness ---

func runLoom(t *testing.T, cwd string, env, args []string, stdin string) (lines []map[string]any, exit int) {
	t.Helper()
	cmd := exec.Command(loomBin, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run loom: %v (stderr=%s)", err, errb.String())
	}
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var mp map[string]any
		if err := json.Unmarshal([]byte(line), &mp); err != nil {
			t.Fatalf("stdout line is not JSON: %q", line)
		}
		lines = append(lines, mp)
	}
	return lines, exit
}

func mockLLMEnv(serverURL string) []string {
	return []string{"LOOM_BASE_URL=" + serverURL, "LOOM_API_KEY=test-key"}
}

func TestE2E_ChatStreaming(t *testing.T) {
	m := &mockLLM{turns: []llmTurn{{content: "Hello world"}}}
	srv := httptest.NewServer(m.handler())
	defer srv.Close()

	lines, exit := runLoom(t, t.TempDir(), mockLLMEnv(srv.URL),
		[]string{"run", "--model", "test", "--cwd", t.TempDir()},
		`{"type":"chat","instruction":"hi"}`)

	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if lines[0]["type"] != "session_init" {
		t.Fatalf("first event: %v", lines[0])
	}
	var texts []string
	for _, l := range lines {
		if l["type"] == "text" {
			texts = append(texts, l["text"].(string))
		}
	}
	if len(texts) < 2 {
		t.Fatalf("expected ≥2 streamed text deltas, got %v", texts)
	}
	if strings.Join(texts, "") != "Hello world" {
		t.Fatalf("streamed text reassembles to %q", strings.Join(texts, ""))
	}
	last := lines[len(lines)-1]
	if last["type"] != "result" || last["status"] != "completed" || last["output"] != "Hello world" {
		t.Fatalf("last event: %v", last)
	}
	// the request actually used the streaming endpoint
	if !strings.Contains(m.requests[0], `"stream":true`) {
		t.Fatalf("expected a streaming request, got: %s", m.requests[0])
	}
}

func TestE2E_ToolFlow(t *testing.T) {
	llm := &mockLLM{turns: []llmTurn{
		{toolCalls: []scriptedToolCall{{id: "c1", name: "list_inventory", args: `{}`}}},
		{content: "found 3 parts"},
	}}
	llmSrv := httptest.NewServer(llm.handler())
	defer llmSrv.Close()

	mcpServer := &mockMCP{tools: []contract.ToolDef{{Name: "list_inventory"}}}
	mcpSrv := httptest.NewServer(mcpServer.handler())
	defer mcpSrv.Close()

	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, ".loom-mcp.json"),
		[]byte(`{"mcpServers":{"sp":{"url":"`+mcpSrv.URL+`","type":"http"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	lines, exit := runLoom(t, cwd, mockLLMEnv(llmSrv.URL),
		[]string{"run", "--model", "test", "--cwd", cwd},
		`{"type":"chat","instruction":"list parts"}`)

	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if !anyEvent(lines, "tool_use") {
		t.Fatalf("missing tool_use: %v", lines)
	}
	if !anyEvent(lines, "tool_result") {
		t.Fatalf("missing tool_result: %v", lines)
	}
	last := lines[len(lines)-1]
	if last["type"] != "result" || last["output"] != "found 3 parts" {
		t.Fatalf("last event: %v", last)
	}
	if mcpServer.lastCall["name"] != "list_inventory" {
		t.Fatalf("MCP server got wrong tool: %v", mcpServer.lastCall["name"])
	}
}

func TestE2E_ResumePersistsAcrossProcesses(t *testing.T) {
	llm := &mockLLM{turns: []llmTurn{{content: "first answer"}, {content: "second answer"}}}
	srv := httptest.NewServer(llm.handler())
	defer srv.Close()

	cwd := t.TempDir()
	env := mockLLMEnv(srv.URL)

	// Turn 1 — capture the session id.
	lines1, _ := runLoom(t, cwd, env, []string{"run", "--model", "test", "--cwd", cwd}, `{"type":"chat","instruction":"first question"}`)
	sid, _ := lines1[len(lines1)-1]["session_id"].(string)
	if sid == "" {
		t.Fatal("no session id from turn 1")
	}

	// Turn 2 — separate process, --resume the same id.
	runLoom(t, cwd, env, []string{"run", "--model", "test", "--cwd", cwd, "--resume", sid}, `{"type":"chat","instruction":"second question"}`)

	// The second process's LLM request must include the prior turn (loaded from
	// the on-disk session) — proving resume works across separate spawns.
	llm.mu.Lock()
	lastReq := llm.requests[len(llm.requests)-1]
	llm.mu.Unlock()
	if !strings.Contains(lastReq, "first question") || !strings.Contains(lastReq, "first answer") {
		t.Fatalf("resume did not reload prior turn into the 2nd request: %s", lastReq)
	}
}

func TestE2E_ErrorPathExitsNonZero(t *testing.T) {
	llm := &mockLLM{turns: []llmTurn{{httpError: 500}}}
	srv := httptest.NewServer(llm.handler())
	defer srv.Close()

	lines, exit := runLoom(t, t.TempDir(), mockLLMEnv(srv.URL),
		[]string{"run", "--model", "test"}, `{"type":"chat","instruction":"hi"}`)

	if exit != 1 {
		t.Fatalf("expected exit 1 on upstream error, got %d", exit)
	}
	if !anyEvent(lines, "error") {
		t.Fatalf("missing error event: %v", lines)
	}
	if lines[len(lines)-1]["status"] != "failed" {
		t.Fatalf("expected result failed: %v", lines[len(lines)-1])
	}
}

func TestE2E_BadProviderConfigStructuredFailure(t *testing.T) {
	// No API key configured for the model → structured failure, not a crash.
	lines, exit := runLoom(t, t.TempDir(),
		[]string{"DEEPSEEK_API_KEY=", "OPENAI_API_KEY=", "LOOM_API_KEY=", "LOOM_BASE_URL="},
		[]string{"run", "--model", "deepseek-chat"}, `{"type":"chat","instruction":"hi"}`)
	if exit != 1 {
		t.Fatalf("exit = %d", exit)
	}
	if !anyEvent(lines, "error") || lines[len(lines)-1]["status"] != "failed" {
		t.Fatalf("expected structured failure: %v", lines)
	}
}

func TestE2E_Version(t *testing.T) {
	cmd := exec.Command(loomBin, "version")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "loom ") {
		t.Fatalf("version output: %q", out)
	}
}

func TestE2E_TextFormatIsHumanReadable(t *testing.T) {
	m := &mockLLM{turns: []llmTurn{{content: "plain answer"}}}
	srv := httptest.NewServer(m.handler())
	defer srv.Close()

	cmd := exec.Command(loomBin, "run", "--model", "test", "--format", "text", "--cwd", t.TempDir())
	cmd.Env = append(os.Environ(), mockLLMEnv(srv.URL)...)
	cmd.Stdin = strings.NewReader(`{"type":"chat","instruction":"hi"}`)
	out, _ := cmd.Output()
	// text mode prints the assistant text and a [completed] marker, NOT JSON
	if !strings.Contains(string(out), "plain answer") || !strings.Contains(string(out), "[completed]") {
		t.Fatalf("text output: %q", out)
	}
	if strings.Contains(string(out), `"type":"session_init"`) {
		t.Fatalf("text mode should not emit JSON events: %q", out)
	}
}

func anyEvent(lines []map[string]any, typ string) bool {
	for _, l := range lines {
		if l["type"] == typ {
			return true
		}
	}
	return false
}
