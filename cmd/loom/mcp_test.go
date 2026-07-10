package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

type recordedMCPRequest struct {
	Method string
	HasID  bool
}

type handlerTransport struct {
	handler http.Handler
}

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}

type errorTransport struct {
	err error
}

func (t errorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.err
}

func newMockMCPHost(name string, mock *mockMCP, allow []string) *mcpHost {
	return newMockMCPHostRequired(name, mock, allow, false)
}

func newMockMCPHostRequired(name string, mock *mockMCP, allow []string, required bool) *mcpHost {
	h := newMCPHost(name, "http://"+name+".test/mcp", allow, required)
	h.client = &http.Client{Transport: handlerTransport{handler: mock.handler()}}
	return h
}

func newUnreachableMCPHost(name string, required bool) *mcpHost {
	h := newMCPHost(name, "http://127.0.0.1:1/mcp", nil, required)
	h.client = &http.Client{Transport: errorTransport{err: errors.New("dial tcp 127.0.0.1:1: connect: connection refused")}}
	return h
}

func attachMockMCP(t *testing.T, d contract.ToolDispatcher, mock *mockMCP) {
	t.Helper()
	md, ok := d.(*mcpDispatcher)
	if !ok {
		t.Fatalf("dispatcher = %T, want *mcpDispatcher", d)
	}
	if len(md.servers) != 1 {
		t.Fatalf("server count = %d, want 1", len(md.servers))
	}
	md.servers[0].client = &http.Client{Transport: handlerTransport{handler: mock.handler()}}
}

// mockMCP is a JSON-RPC 2.0 MCP server for tests. It advertises `tools` and
// echoes tool calls. It records the last tools/call params.
type mockMCP struct {
	tools          []contract.ToolDef
	failInitialize bool
	failList       bool

	mu       sync.Mutex
	requests []recordedMCPRequest
	lastCall map[string]any
}

func (m *mockMCP) record(method string, hasID bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, recordedMCPRequest{Method: method, HasID: hasID})
}

func (m *mockMCP) requestMethods() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	methods := make([]string, 0, len(m.requests))
	for _, r := range m.requests {
		methods = append(methods, r.Method)
	}
	return methods
}

func (m *mockMCP) requestAt(i int) recordedMCPRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests[i]
}

func (m *mockMCP) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var method string
		_ = json.Unmarshal(raw["method"], &method)
		id, hasID := raw["id"]
		m.record(method, hasID)

		w.Header().Set("Content-Type", "application/json")

		switch method {
		case "initialize":
			if m.failInitialize {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0", "id": id,
					"error": map[string]any{"code": -32000, "message": "initialize failed"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "mock", "version": "test"},
				},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			if m.failList {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0", "id": id,
					"error": map[string]any{"code": -32000, "message": "list failed"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{"tools": m.tools},
			})
		case "tools/call":
			_ = json.Unmarshal(raw["params"], &m.lastCall)
			name, _ := m.lastCall["name"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": "ran " + name}},
					"isError": false,
				},
			})
		default:
			http.Error(w, "method not found", http.StatusBadRequest)
		}
	}
}

func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	old := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestLoadMCPDispatcherMissingFileReturnsNoTools(t *testing.T) {
	assertNoMCPTools(t, filepath.Join(t.TempDir(), "nope.json"))
}

func TestLoadMCPDispatcherEmptyConfigReturnsNoTools(t *testing.T) {
	assertNoMCPTools(t, writeMCPConfig(t, `{"mcpServers":{}}`))
}

func assertNoMCPTools(t *testing.T, path string) {
	t.Helper()
	d, err := loadMCPDispatcher(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.(noTools); !ok {
		t.Fatalf("missing config should yield noTools, got %T", d)
	}
	tools, err := d.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Fatalf("tools = %+v, want none", tools)
	}
	if _, ok := d.(interface{ Preflight(context.Context) error }); ok {
		t.Fatalf("noTools must not implement Preflight")
	}
}

func TestLoadMCPDispatcherParsesRequired(t *testing.T) {
	tests := []struct {
		name         string
		serverConfig string
		wantRequired bool
	}{
		{name: "required", serverConfig: `{"url":"http://sp.test/mcp","required":true}`, wantRequired: true},
		{name: "defaults optional", serverConfig: `{"url":"http://sp.test/mcp"}`, wantRequired: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := writeMCPConfig(t, `{"mcpServers":{"sp":`+tt.serverConfig+`}}`)
			d, err := loadMCPDispatcher(p)
			if err != nil {
				t.Fatal(err)
			}
			md, ok := d.(*mcpDispatcher)
			if !ok {
				t.Fatalf("dispatcher = %T, want *mcpDispatcher", d)
			}
			if len(md.servers) != 1 {
				t.Fatalf("server count = %d, want 1", len(md.servers))
			}
			if got := md.servers[0].required; got != tt.wantRequired {
				t.Fatalf("required = %v, want %v", got, tt.wantRequired)
			}
		})
	}
}

func writeMCPConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".loom-mcp.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMCPHostInitializesBeforeListTools(t *testing.T) {
	mock := &mockMCP{tools: []contract.ToolDef{{Name: "lookup_part"}}}

	h := newMockMCPHost("sp", mock, nil)
	tools, err := h.listTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "lookup_part" {
		t.Fatalf("bad tools: %+v", tools)
	}

	want := []string{"initialize", "notifications/initialized", "tools/list"}
	if got := mock.requestMethods(); !reflect.DeepEqual(got, want) {
		t.Fatalf("method order = %v, want %v", got, want)
	}
	if req := mock.requestAt(1); req.HasID {
		t.Fatalf("notifications/initialized should be a notification without id: %+v", req)
	}
}

func TestLoadMCPDispatcherRejectsMissingURL(t *testing.T) {
	p := writeMCPConfig(t, `{"mcpServers":{"x":{"type":"http"}}}`)
	if _, err := loadMCPDispatcher(p); err == nil {
		t.Fatal("want error for server missing url")
	}
}

func TestLoadMCPDispatcherRejectsUnsupportedType(t *testing.T) {
	p := writeMCPConfig(t, `{"mcpServers":{"x":{"url":"http://h","type":"stdio"}}}`)
	if _, err := loadMCPDispatcher(p); err == nil {
		t.Fatal("want error for unsupported type")
	}
}

func TestMCPDispatcherListAndDispatch(t *testing.T) {
	mock := &mockMCP{tools: []contract.ToolDef{
		{Name: "list_inventory", Description: "list parts"},
		{Name: "create_order", Description: "order parts"},
	}}

	p := writeMCPConfig(t, `{"mcpServers":{"sp":{"url":"http://sp.test/mcp","type":"http"}}}`)
	d, err := loadMCPDispatcher(p)
	if err != nil {
		t.Fatal(err)
	}
	attachMockMCP(t, d, mock)

	tools, err := d.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("want 2 tools, got %d", len(tools))
	}

	res, err := d.Dispatch(context.Background(), contract.ToolCall{ID: "c1", Name: "create_order", Args: `{"sku":"A"}`})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.Content != "ran create_order" {
		t.Fatalf("bad dispatch result: %+v", res)
	}
	if mock.lastCall["name"] != "create_order" {
		t.Fatalf("server got wrong tool name: %v", mock.lastCall["name"])
	}
}

func TestMCPDispatcherFilterHidesAndBlocks(t *testing.T) {
	mock := &mockMCP{tools: []contract.ToolDef{
		{Name: "list_inventory"},
		{Name: "delete_everything"},
	}}

	// allowlist only list_inventory
	p := writeMCPConfig(t, `{"mcpServers":{"sp":{"url":"http://sp.test/mcp","type":"http","tools":["list_inventory"]}}}`)
	d, err := loadMCPDispatcher(p)
	if err != nil {
		t.Fatal(err)
	}
	attachMockMCP(t, d, mock)

	tools, _ := d.ListTools(context.Background())
	if len(tools) != 1 || tools[0].Name != "list_inventory" {
		t.Fatalf("filter should expose only list_inventory: %+v", tools)
	}

	// dispatching a filtered-out tool must be blocked (IsError), not forwarded
	res, _ := d.Dispatch(context.Background(), contract.ToolCall{ID: "c", Name: "delete_everything", Args: `{}`})
	if !res.IsError {
		t.Fatalf("blocked tool should return IsError, got %+v", res)
	}
}

func TestMCPDispatcherUnknownToolIsError(t *testing.T) {
	mock := &mockMCP{tools: []contract.ToolDef{{Name: "known"}}}

	p := writeMCPConfig(t, `{"mcpServers":{"sp":{"url":"http://sp.test/mcp","type":"http"}}}`)
	d, _ := loadMCPDispatcher(p)
	attachMockMCP(t, d, mock)
	res, err := d.Dispatch(context.Background(), contract.ToolCall{ID: "c", Name: "nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("unknown tool should be IsError: %+v", res)
	}
}

func TestMCPDispatcherSkipsFailedServers(t *testing.T) {
	tests := []struct {
		name string
		bad  *mockMCP
	}{
		{name: "list failure", bad: &mockMCP{failList: true}},
		{name: "initialize failure", bad: &mockMCP{failInitialize: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			captureSlog(t)
			good := &mockMCP{tools: []contract.ToolDef{{Name: "lookup_part"}}}

			d := &mcpDispatcher{servers: []*mcpHost{
				newMockMCPHost("bad", tt.bad, nil),
				newMockMCPHost("good", good, nil),
			}}
			tools, err := d.ListTools(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(tools) != 1 || tools[0].Name != "lookup_part" {
				t.Fatalf("tools = %+v, want only lookup_part", tools)
			}
		})
	}
}

func TestMCPDispatcherRequiredFailureIsError(t *testing.T) {
	tests := []struct {
		name       string
		newHost    func() *mcpHost
		wantReason string
	}{
		{
			name:       "connection refused",
			newHost:    func() *mcpHost { return newUnreachableMCPHost("weave-dispatch", true) },
			wantReason: "connection refused",
		},
		{
			name: "initialize JSON-RPC error",
			newHost: func() *mcpHost {
				return newMockMCPHostRequired("weave-dispatch", &mockMCP{failInitialize: true}, nil, true)
			},
			wantReason: "initialize failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			captureSlog(t)
			d := &mcpDispatcher{servers: []*mcpHost{
				tt.newHost(),
				newMockMCPHost("healthy", &mockMCP{tools: []contract.ToolDef{{Name: "healthy_tool"}}}, nil),
			}}
			_, err := d.ListTools(context.Background())
			assertMCPServerError(t, err, "weave-dispatch", tt.wantReason)
			if d.listed {
				t.Fatal("failed listing must not be cached")
			}

			preflight, ok := any(d).(interface{ Preflight(context.Context) error })
			if !ok {
				t.Fatal("mcpDispatcher must implement Preflight")
			}
			assertMCPServerError(t, preflight.Preflight(context.Background()), "weave-dispatch", tt.wantReason)
		})
	}
}

func TestMCPDispatcherSkipsUnreachableOptionalServerWhenAnotherLoads(t *testing.T) {
	logs := captureSlog(t)
	good := &mockMCP{tools: []contract.ToolDef{{Name: "lookup_part"}}}
	d := &mcpDispatcher{servers: []*mcpHost{
		newUnreachableMCPHost("optional", false),
		newMockMCPHost("healthy", good, nil),
	}}

	tools, err := d.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "lookup_part" {
		t.Fatalf("tools = %+v, want only lookup_part", tools)
	}
	if got := logs.String(); !strings.Contains(got, "server skipped") || !strings.Contains(got, "optional") {
		t.Fatalf("logs missing skipped optional server: %s", got)
	}
}

func TestMCPDispatcherAllOptionalServersFailedIsError(t *testing.T) {
	captureSlog(t)
	d := &mcpDispatcher{servers: []*mcpHost{
		newUnreachableMCPHost("one", false),
		newUnreachableMCPHost("two", false),
	}}

	_, err := d.ListTools(context.Background())
	if err == nil {
		t.Fatal("want error when all configured servers fail")
	}
	for _, want := range []string{"all configured MCP servers failed", "one", "two", "connection refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want substring %q", err, want)
		}
	}
	if d.listed {
		t.Fatal("all-failed listing must not be cached")
	}
}

func TestMCPDispatcherLoadedEmptyServersAreNotAllFailed(t *testing.T) {
	logs := captureSlog(t)
	d := &mcpDispatcher{servers: []*mcpHost{
		newMockMCPHostRequired("required-empty", &mockMCP{}, nil, true),
		newMockMCPHost("optional-empty", &mockMCP{}, nil),
	}}

	tools, err := d.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Fatalf("tools = %+v, want none", tools)
	}
	if got := logs.String(); !strings.Contains(got, "level=ERROR") || !strings.Contains(got, "loaded zero tools") {
		t.Fatalf("logs missing zero-tools error: %s", got)
	}
}

func TestMCPDispatcherFailureIsRetried(t *testing.T) {
	captureSlog(t)
	mock := &mockMCP{tools: []contract.ToolDef{{Name: "recovered"}}, failList: true}
	d := &mcpDispatcher{servers: []*mcpHost{newMockMCPHost("recovering", mock, nil)}}

	if _, err := d.ListTools(context.Background()); err == nil {
		t.Fatal("want initial all-failed error")
	}
	mock.failList = false
	tools, err := d.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "recovered" {
		t.Fatalf("tools = %+v, want recovered", tools)
	}
}

func assertMCPServerError(t *testing.T, err error, server, reason string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error for server %q", server)
	}
	for _, want := range []string{`required server "` + server + `" failed`, reason} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want substring %q", err, want)
		}
	}
}
