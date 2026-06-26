package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/jinyitao123/loom/contract"
)

// .loom-mcp.json — written by the weave daemon (writeMcpConfigFiles, C.7) from
// the agent's mcpServers, read here from the agent workdir. v1 supports HTTP
// JSON-RPC MCP servers only (the same wire protocol as weave/internal/mcphost
// and the Spare-parts backend).
//
//	{ "mcpServers": { "<name>": { "url": "http://…", "type": "http",
//	                              "tools": ["allow1","allow2"] } } }
type mcpConfig struct {
	MCPServers map[string]mcpServerConfig `json:"mcpServers"`
}

type mcpServerConfig struct {
	URL   string   `json:"url"`
	Type  string   `json:"type"`
	Tools []string `json:"tools"` // optional allowlist
}

// loadMCPDispatcher reads path and returns a tool dispatcher. A missing file is
// not an error — it yields noTools (the agent simply has no tools).
func loadMCPDispatcher(path string) (contract.ToolDispatcher, error) {
	// #nosec G304 -- path is the operator/daemon-supplied .loom-mcp.json in the
	// agent workdir, not untrusted remote input.
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return noTools{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg mcpConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var servers []*mcpHost
	for name, sc := range cfg.MCPServers {
		if sc.URL == "" {
			return nil, fmt.Errorf("mcp server %q: missing url", name)
		}
		if sc.Type != "" && sc.Type != "http" {
			return nil, fmt.Errorf("mcp server %q: unsupported type %q (v1 supports http)", name, sc.Type)
		}
		servers = append(servers, newMCPHost(name, sc.URL, sc.Tools))
	}
	if len(servers) == 0 {
		return noTools{}, nil
	}
	return &mcpDispatcher{servers: servers}, nil
}

// mcpHost is a single-server JSON-RPC MCP client implementing the tools/list +
// tools/call methods, with an optional tool allowlist.
type mcpHost struct {
	name    string
	baseURL string
	client  *http.Client
	filter  map[string]bool
}

func newMCPHost(name, baseURL string, allow []string) *mcpHost {
	h := &mcpHost{name: name, baseURL: baseURL, client: &http.Client{Timeout: 30 * time.Second}}
	if len(allow) > 0 {
		h.filter = make(map[string]bool, len(allow))
		for _, n := range allow {
			h.filter[n] = true
		}
	}
	return h
}

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	ID      int    `json:"id"`
}

type jsonRPCResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (h *mcpHost) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	data, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", Method: method, Params: params, ID: 1})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp %s call %s: %w", h.name, method, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var rpc jsonRPCResponse
	if err := json.Unmarshal(body, &rpc); err != nil {
		return nil, fmt.Errorf("mcp %s response parse: %w", h.name, err)
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("mcp %s error %d: %s", h.name, rpc.Error.Code, rpc.Error.Message)
	}
	return rpc.Result, nil
}

func (h *mcpHost) listTools(ctx context.Context) ([]contract.ToolDef, error) {
	result, err := h.call(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tools []contract.ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, err
	}
	if h.filter == nil {
		return resp.Tools, nil
	}
	var filtered []contract.ToolDef
	for _, t := range resp.Tools {
		if h.filter[t.Name] {
			filtered = append(filtered, t)
		}
	}
	return filtered, nil
}

func (h *mcpHost) dispatch(ctx context.Context, call contract.ToolCall) *contract.ToolResult {
	if h.filter != nil && !h.filter[call.Name] {
		return &contract.ToolResult{CallID: call.ID, Content: fmt.Sprintf("tool %q is not available for this agent", call.Name), IsError: true}
	}
	result, err := h.call(ctx, "tools/call", map[string]any{"name": call.Name, "arguments": json.RawMessage(call.Args)})
	if err != nil {
		return &contract.ToolResult{CallID: call.ID, Content: err.Error(), IsError: true}
	}
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return &contract.ToolResult{CallID: call.ID, Content: string(result)}
	}
	var content string
	for _, c := range resp.Content {
		if c.Type == "text" {
			content += c.Text
		}
	}
	return &contract.ToolResult{CallID: call.ID, Content: content, IsError: resp.IsError}
}

// mcpDispatcher aggregates one or more MCP servers behind contract.ToolDispatcher,
// routing each tool call to the server that advertised it (first server wins on
// a name collision).
type mcpDispatcher struct {
	servers []*mcpHost

	mu     sync.Mutex
	listed bool
	tools  []contract.ToolDef
	route  map[string]*mcpHost
}

func (d *mcpDispatcher) ensureListed(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.listed {
		return nil
	}
	route := make(map[string]*mcpHost)
	var all []contract.ToolDef
	for _, s := range d.servers {
		ts, err := s.listTools(ctx)
		if err != nil {
			return err
		}
		for _, t := range ts {
			if _, dup := route[t.Name]; dup {
				continue
			}
			route[t.Name] = s
			all = append(all, t)
		}
	}
	d.route, d.tools, d.listed = route, all, true
	return nil
}

func (d *mcpDispatcher) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	if err := d.ensureListed(ctx); err != nil {
		return nil, err
	}
	return d.tools, nil
}

func (d *mcpDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if err := d.ensureListed(ctx); err != nil {
		return nil, err
	}
	d.mu.Lock()
	s := d.route[call.Name]
	d.mu.Unlock()
	if s == nil {
		return &contract.ToolResult{CallID: call.ID, Content: fmt.Sprintf("unknown tool %q", call.Name), IsError: true}, nil
	}
	return s.dispatch(ctx, call), nil
}

var _ contract.ToolDispatcher = (*mcpDispatcher)(nil)
