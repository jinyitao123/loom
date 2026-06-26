package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

// mockMCP is a JSON-RPC 2.0 MCP server for tests. It advertises `tools` and
// echoes tool calls. It records the last tools/call params.
type mockMCP struct {
	tools    []contract.ToolDef
	lastCall map[string]any
}

func (m *mockMCP) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			ID     int             `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")

		switch req.Method {
		case "tools/list":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": m.tools},
			})
		case "tools/call":
			_ = json.Unmarshal(req.Params, &m.lastCall)
			name, _ := m.lastCall["name"].(string)
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
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

func TestLoadMCPDispatcherMissingFileReturnsNoTools(t *testing.T) {
	d, err := loadMCPDispatcher(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.(noTools); !ok {
		t.Fatalf("missing config should yield noTools, got %T", d)
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
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	p := writeMCPConfig(t, `{"mcpServers":{"sp":{"url":"`+srv.URL+`","type":"http"}}}`)
	d, err := loadMCPDispatcher(p)
	if err != nil {
		t.Fatal(err)
	}

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
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	// allowlist only list_inventory
	p := writeMCPConfig(t, `{"mcpServers":{"sp":{"url":"`+srv.URL+`","type":"http","tools":["list_inventory"]}}}`)
	d, err := loadMCPDispatcher(p)
	if err != nil {
		t.Fatal(err)
	}

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
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	p := writeMCPConfig(t, `{"mcpServers":{"sp":{"url":"`+srv.URL+`","type":"http"}}}`)
	d, _ := loadMCPDispatcher(p)
	res, err := d.Dispatch(context.Background(), contract.ToolCall{ID: "c", Name: "nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("unknown tool should be IsError: %+v", res)
	}
}
