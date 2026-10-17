package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
)

// TestMain doubles as a fake stdio MCP server when LOOM_FAKE_MCP is set.
func TestMain(m *testing.M) {
	if os.Getenv("LOOM_FAKE_MCP") == "1" {
		fakeStdioServer(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeServer answers MCP requests; ask() sends an elicitation and waits.
type fakeServer struct {
	mu        sync.Mutex
	write     func(any)
	nextID    int
	waiting   map[string]chan json.RawMessage
	listCalls int
}

func (f *fakeServer) handle(msg map[string]any) any {
	id := msg["id"]
	method, _ := msg["method"].(string)
	if method == "" { // a response to our elicitation
		f.mu.Lock()
		ch := f.waiting[fmt.Sprint(id)]
		f.mu.Unlock()
		raw, _ := json.Marshal(msg["result"])
		if ch != nil {
			ch <- raw
		}
		return nil
	}
	if id == nil { // notification
		return nil
	}
	reply := func(result any) any { return map[string]any{"jsonrpc": "2.0", "id": id, "result": result} }
	switch method {
	case "initialize":
		params := msg["params"].(map[string]any)
		caps := params["capabilities"].(map[string]any)
		if _, ok := caps["elicitation"]; !ok {
			return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -1, "message": "client lacks elicitation"}}
		}
		return reply(map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{"listChanged": true}}, "serverInfo": map[string]any{"name": "fake"}, "instructions": "Use echo for greetings."})
	case "tools/list":
		f.mu.Lock()
		f.listCalls++
		calls := f.listCalls
		f.mu.Unlock()
		tools := []any{
			map[string]any{"name": "echo", "description": "Echo text", "inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{"readOnlyHint": true}},
			map[string]any{"name": "ask.color", "description": "Ask the user", "inputSchema": map[string]any{"type": "object"}},
		}
		if calls > 1 {
			tools = append(tools, map[string]any{"name": "late", "description": "Added later"})
		}
		return reply(map[string]any{"tools": tools})
	case "tools/call":
		params := msg["params"].(map[string]any)
		args, _ := json.Marshal(params["arguments"])
		switch params["name"] {
		case "echo":
			f.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
			return reply(map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "echo " + string(args)},
				map[string]any{"type": "image", "data": "AAAA", "mimeType": "image/png"},
			}})
		case "ask.color":
			answer := f.elicit()
			return reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": "user said " + string(answer)}}, "isError": false})
		}
		return reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": "unknown tool"}}, "isError": true})
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": "no " + method}}
}

func (f *fakeServer) elicit() json.RawMessage {
	f.mu.Lock()
	f.nextID++
	id := fmt.Sprintf("e%d", f.nextID)
	ch := make(chan json.RawMessage, 1)
	if f.waiting == nil {
		f.waiting = map[string]chan json.RawMessage{}
	}
	f.waiting[id] = ch
	f.mu.Unlock()
	f.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": "elicitation/create", "params": map[string]any{
		"message": "Pick a color",
		"requestedSchema": map[string]any{"type": "object", "properties": map[string]any{
			"color": map[string]any{"type": "string", "enum": []any{"red", "blue"}},
		}},
	}})
	return <-ch
}

func fakeStdioServer(in io.Reader, out io.Writer) {
	var mu sync.Mutex
	f := &fakeServer{}
	f.write = func(v any) {
		line, _ := json.Marshal(v)
		mu.Lock()
		defer mu.Unlock()
		_, _ = out.Write(append(line, '\n'))
	}
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		var msg map[string]any
		if json.Unmarshal(scanner.Bytes(), &msg) != nil {
			continue
		}
		go func(msg map[string]any) {
			if reply := f.handle(msg); reply != nil {
				f.write(reply)
			}
		}(msg)
	}
}

var askedFor []string

func testElicit(_ context.Context, server, message string, schema json.RawMessage) (ElicitResult, error) {
	askedFor = append(askedFor, server+": "+message)
	return ElicitResult{Action: "accept", Content: map[string]any{"color": "blue"}}, nil
}

func exercise(t *testing.T, m *Manager, prefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defs, err := m.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 2 || defs[0].Name != prefix+"echo" || !defs[0].ReadOnly || defs[1].Name != prefix+"ask_color" || defs[1].ReadOnly {
		t.Fatalf("tools = %+v", defs)
	}
	if !strings.Contains(m.Instructions(), "Use echo for greetings.") {
		t.Fatalf("instructions = %q", m.Instructions())
	}
	r, err := m.Dispatch(ctx, contract.ToolCall{ID: "1", Name: prefix + "echo", Args: `{"text":"hi"}`})
	if err != nil || r.IsError || r.Content != `echo {"text":"hi"}` || len(r.Parts) != 2 || r.Parts[1].Type != "image" {
		t.Fatalf("echo = %+v, %v", r, err)
	}
	askedFor = nil
	r, err = m.Dispatch(ctx, contract.ToolCall{ID: "2", Name: prefix + "ask_color", Args: `{}`})
	if err != nil || r.Content != `user said {"action":"accept","content":{"color":"blue"}}` {
		t.Fatalf("ask = %+v, %v", r, err)
	}
	if len(askedFor) != 1 || askedFor[0] != "fake: Pick a color" {
		t.Fatalf("elicitations = %v", askedFor)
	}
	// echo announced a changed tool list; the next listing refreshes it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		defs, _ = m.ListTools(ctx)
		if len(defs) == 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(defs) != 3 || defs[2].Name != prefix+"late" {
		t.Fatalf("tools after list_changed = %+v", defs)
	}
	if r, _ := m.Dispatch(ctx, contract.ToolCall{ID: "3", Name: "nope"}); !r.IsError {
		t.Fatal("unknown tool dispatched")
	}
}

func TestStdioServer(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m, failures := Connect(context.Background(), map[string]ServerConfig{
		"fake":   {Command: exe, Env: map[string]string{"LOOM_FAKE_MCP": "1"}},
		"broken": {Command: "loom-no-such-binary"},
		"off":    {Command: exe, Disabled: true},
	}, Options{ClientVersion: "test", Elicit: testElicit, Timeout: 5 * time.Second})
	defer m.Close()
	if len(failures) != 1 || !strings.Contains(failures[0].Error(), "broken") {
		t.Fatalf("failures = %v", failures)
	}
	if got := m.Servers(); len(got) != 1 || got[0] != "fake" {
		t.Fatalf("servers = %v", got)
	}
	exercise(t, m, "mcp__fake__")
}

// fakeHTTP serves streamable HTTP: tool calls answer with an event stream so
// the elicitation request can travel before the result.
func fakeHTTP(t *testing.T) (*httptest.Server, *[]string) {
	var sessions []string
	f := &fakeServer{}
	var streamMu sync.Mutex
	var stream func(any)
	f.write = func(v any) {
		streamMu.Lock()
		defer streamMu.Unlock()
		if stream != nil {
			stream(v)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		sessions = append(sessions, r.Header.Get("Mcp-Session-Id"))
		var msg map[string]any
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if msg["method"] == nil || msg["id"] == nil {
			f.handle(msg)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if msg["method"] == "tools/call" {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			done := make(chan struct{})
			streamMu.Lock()
			stream = func(v any) {
				line, _ := json.Marshal(v)
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", line)
				flusher.Flush()
			}
			streamMu.Unlock()
			go func() {
				reply := f.handle(msg)
				f.write(reply)
				close(done)
			}()
			<-done
			streamMu.Lock()
			stream = nil
			streamMu.Unlock()
			return
		}
		reply := f.handle(msg)
		if msg["method"] == "initialize" {
			w.Header().Set("Mcp-Session-Id", "s-1")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	return srv, &sessions
}

func TestHTTPServer(t *testing.T) {
	srv, sessions := fakeHTTP(t)
	defer srv.Close()
	t.Setenv("FAKE_TOKEN", "secret")
	prefix := ""
	m, failures := Connect(context.Background(), map[string]ServerConfig{
		"fake": {URL: srv.URL + "/mcp", Headers: map[string]string{"Authorization": "Bearer ${FAKE_TOKEN}"}, ToolPrefix: &prefix},
	}, Options{ClientVersion: "test", Elicit: testElicit, Timeout: 5 * time.Second})
	defer m.Close()
	if len(failures) != 0 {
		t.Fatalf("failures = %v", failures)
	}
	exercise(t, m, "")
	if (*sessions)[0] != "" || (*sessions)[len(*sessions)-1] != "s-1" {
		t.Fatalf("session ids sent = %v", *sessions)
	}
}

func TestLoadConfigLaterFilesWin(t *testing.T) {
	dir := t.TempDir()
	a := dir + "/a.json"
	b := dir + "/b.json"
	_ = os.WriteFile(a, []byte(`{"mcpServers":{"x":{"command":"one"},"y":{"url":"http://y"}}}`), 0o600)
	_ = os.WriteFile(b, []byte(`{"mcpServers":{"x":{"command":"two"}}}`), 0o600)
	servers, err := LoadConfig(a, dir+"/missing.json", b)
	if err != nil {
		t.Fatal(err)
	}
	if servers["x"].Command != "two" || servers["y"].URL != "http://y" {
		t.Fatalf("servers = %+v", servers)
	}
}
