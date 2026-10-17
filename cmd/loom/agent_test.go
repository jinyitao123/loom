package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAgentCmdAnswersOverStdio drives `loom agent` end to end: models.json
// points at a local OpenAI-compatible server, the prompt's answer streams back
// as message_update events, and the session file lands under the session root.
func TestAgentCmdAnswersOverStdio(t *testing.T) {
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"content":" there"},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	agentDir := t.TempDir()
	workdir := t.TempDir()
	models := fmt.Sprintf(`{"providers":{"local":{"baseUrl":%q,"api":"openai-completions","apiKey":"TEST_KEY","models":[{"id":"m"}]}}}`, server.URL+"/v1")
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"LOOM_AGENT_DIR": agentDir, "TEST_KEY": "secret"}
	getenv := func(name string) string { return env[name] }

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var stderr bytes.Buffer
	exit := make(chan int, 1)
	go func() {
		exit <- agentCmdIO([]string{"--mode", "rpc", "--cwd", workdir, "--tools", "read,ls"}, inR, outW, &stderr, getenv)
		_ = outW.Close()
	}()
	lines := make(chan map[string]any, 100)
	go func() {
		scanner := bufio.NewScanner(outR)
		for scanner.Scan() {
			var v map[string]any
			if json.Unmarshal(scanner.Bytes(), &v) == nil {
				lines <- v
			}
		}
		close(lines)
	}()

	if _, err := io.WriteString(inW, `{"id":"1","type":"prompt","message":"hello"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var sessionFile string
	timeout := time.After(10 * time.Second)
wait:
	for {
		select {
		case v, ok := <-lines:
			if !ok {
				t.Fatalf("output closed early; stderr: %s", stderr.String())
			}
			if v["type"] == "message_update" {
				if e := v["assistantMessageEvent"].(map[string]any); e["type"] == "text_delta" {
					text.WriteString(e["delta"].(string))
				}
			}
			if v["type"] == "agent_settled" {
				break wait
			}
		case <-timeout:
			t.Fatalf("no answer; stderr: %s", stderr.String())
		}
	}
	if _, err := io.WriteString(inW, `{"id":"2","type":"get_state"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	for v := range lines {
		if v["id"] == "2" {
			sessionFile = v["data"].(map[string]any)["sessionFile"].(string)
			break
		}
	}
	_ = inW.Close()
	if code := <-exit; code != 0 {
		t.Fatalf("exit code %d; stderr: %s", code, stderr.String())
	}

	if text.String() != "hi there" {
		t.Fatalf("streamed text = %q", text.String())
	}
	if gotPath != "/v1/chat/completions" || gotAuth != "Bearer secret" {
		t.Fatalf("request path %q auth %q", gotPath, gotAuth)
	}
	wantDir := filepath.Join(agentDir, "sessions")
	if !strings.HasPrefix(sessionFile, wantDir) {
		t.Fatalf("session file %q is not under %q", sessionFile, wantDir)
	}
	if _, err := os.Stat(sessionFile); err != nil {
		t.Fatalf("session file not written: %v", err)
	}
}

func TestAgentCmdRejectsUnknownModeAndTool(t *testing.T) {
	getenv := func(name string) string {
		if name == "LOOM_AGENT_DIR" {
			return t.TempDir()
		}
		return ""
	}
	var stderr bytes.Buffer
	if code := agentCmdIO([]string{"--mode", "json"}, strings.NewReader(""), io.Discard, &stderr, getenv); code != 2 {
		t.Fatalf("unknown mode exit = %d", code)
	}
	if code := agentCmdIO([]string{"--tools", "teleport"}, strings.NewReader(""), io.Discard, &stderr, getenv); code != 2 {
		t.Fatalf("unknown tool exit = %d", code)
	}
}

// A server that cannot start is reported as an extension_error; the agent
// still answers commands.
func TestAgentCmdReportsUnavailableMCPServer(t *testing.T) {
	agentDir := t.TempDir()
	config := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(config, []byte(`{"mcpServers":{"gone":{"command":"loom-no-such-mcp-server"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(name string) string {
		if name == "LOOM_AGENT_DIR" {
			return agentDir
		}
		return ""
	}
	var out, stderr bytes.Buffer
	in := strings.NewReader(`{"id":"1","type":"get_state"}` + "\n")
	if code := agentCmdIO([]string{"--cwd", t.TempDir(), "--mcp-config", config}, in, &out, &stderr, getenv); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(out.String(), `"type":"extension_error"`) || !strings.Contains(out.String(), "gone") {
		t.Fatalf("no extension_error for the unavailable server:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"command":"get_state"`) || !strings.Contains(out.String(), `"success":true`) {
		t.Fatalf("get_state did not answer:\n%s", out.String())
	}
	if !strings.Contains(stderr.String(), "gone") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
