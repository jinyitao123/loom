package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jinyitao123/loom/contract"
)

// ServerConfig describes one server: Command (stdio) or URL (streamable
// HTTP). ${VAR} references in command, args, env, url and headers expand from
// the environment. ToolPrefix overrides the default "mcp__<server>__" prefix
// of tool names; an empty string keeps the server's own names.
// ServerConfig 描述一个服务器：Command（stdio）或 URL（streamable HTTP）；
// 各字段中的 ${VAR} 从环境变量展开；ToolPrefix 覆盖默认工具名前缀，空串表示沿用服务器原名。
type ServerConfig struct {
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Cwd        string            `json:"cwd,omitempty"`
	URL        string            `json:"url,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	ToolPrefix *string           `json:"toolPrefix,omitempty"`
	Disabled   bool              `json:"disabled,omitempty"`
}

// LoadConfig reads {"mcpServers": {...}} files in order; a later file
// replaces a server of the same name. Missing files are skipped.
func LoadConfig(paths ...string) (map[string]ServerConfig, error) {
	servers := map[string]ServerConfig{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var file struct {
			MCPServers map[string]ServerConfig `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &file); err != nil {
			return nil, fmt.Errorf("mcpclient: parse %s: %w", path, err)
		}
		for name, cfg := range file.MCPServers {
			servers[name] = cfg
		}
	}
	return servers, nil
}

// Options configures Connect.
type Options struct {
	ClientVersion string        // reported to servers
	Cwd           string        // working directory of stdio servers without their own
	Elicit        Elicitor      // answers requests for user input; nil cancels them
	Timeout       time.Duration // per-server connection timeout; <=0 means 30s
}

// Manager is a contract.ToolDispatcher over every connected server.
type Manager struct {
	mu      sync.Mutex
	servers []*server
	routes  map[string]route
}

type server struct {
	name    string
	prefix  string
	session *session
	mu      sync.Mutex
	tools   []tool
	stale   bool
}

type route struct {
	server *server
	tool   string
}

// Connect starts every enabled server concurrently. Servers that fail to
// start or initialize are left out and reported in the returned errors.
func Connect(ctx context.Context, configs map[string]ServerConfig, opts Options) (*Manager, []error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	names := make([]string, 0, len(configs))
	for name, cfg := range configs {
		if !cfg.Disabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	results := make([]*server, len(names))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			results[i], errs[i] = connect(ctx, name, configs[name], opts)
		}(i, name)
	}
	wg.Wait()
	m := &Manager{routes: map[string]route{}}
	var failures []error
	for i, s := range results {
		if errs[i] != nil {
			failures = append(failures, fmt.Errorf("MCP server %s: %w", names[i], errs[i]))
			continue
		}
		m.servers = append(m.servers, s)
	}
	return m, failures
}

func connect(ctx context.Context, name string, cfg ServerConfig, opts Options) (*server, error) {
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	s := &server{name: name, prefix: "mcp__" + sanitize(name) + "__"}
	if cfg.ToolPrefix != nil {
		s.prefix = sanitize(*cfg.ToolPrefix)
	}
	sess := newSession(name, opts.Elicit)
	sess.onListChange = func() { s.mu.Lock(); s.stale = true; s.mu.Unlock() }
	switch {
	case cfg.Command != "":
		args := make([]string, len(cfg.Args))
		for i, a := range cfg.Args {
			args[i] = os.ExpandEnv(a)
		}
		env := map[string]string{}
		for k, v := range cfg.Env {
			env[k] = os.ExpandEnv(v)
		}
		dir := cfg.Cwd
		if dir == "" {
			dir = opts.Cwd
		}
		t, err := openStdio(os.ExpandEnv(cfg.Command), args, env, dir, sess)
		if err != nil {
			return nil, err
		}
		sess.transport = t
	case cfg.URL != "":
		headers := map[string]string{}
		for k, v := range cfg.Headers {
			headers[k] = os.ExpandEnv(v)
		}
		sess.transport = openHTTP(os.ExpandEnv(cfg.URL), headers, sess)
	default:
		return nil, errors.New("neither command nor url is set")
	}
	if err := sess.initialize(ctx, opts.ClientVersion); err != nil {
		_ = sess.transport.close()
		return nil, err
	}
	tools, err := sess.listTools(ctx)
	if err != nil {
		_ = sess.transport.close()
		return nil, err
	}
	s.session, s.tools = sess, tools
	return s, nil
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func sanitize(name string) string { return unsafeName.ReplaceAllString(name, "_") }

// Instructions returns the servers' instructions as system prompt sections.
func (m *Manager) Instructions() string {
	var b strings.Builder
	for _, s := range m.servers {
		if text := strings.TrimSpace(s.session.instructions); text != "" {
			fmt.Fprintf(&b, "\n\n# Instructions from the %s tools\n\n%s", s.name, text)
		}
	}
	return b.String()
}

// Servers lists the connected server names.
func (m *Manager) Servers() []string {
	names := make([]string, len(m.servers))
	for i, s := range m.servers {
		names[i] = s.name
	}
	return names
}

// ListTools returns every server's tools, refreshing servers that reported a
// changed list.
func (m *Manager) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var defs []contract.ToolDef
	m.routes = map[string]route{}
	for _, s := range m.servers {
		s.mu.Lock()
		stale := s.stale
		s.stale = false
		s.mu.Unlock()
		if stale {
			if tools, err := s.session.listTools(ctx); err == nil {
				s.tools = tools
			}
		}
		for _, t := range s.tools {
			name := s.prefix + sanitize(t.Name)
			if len(name) > 64 {
				name = name[:64]
			}
			if _, taken := m.routes[name]; taken {
				continue
			}
			schema := t.InputSchema
			if len(schema) == 0 || string(schema) == "null" {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			m.routes[name] = route{server: s, tool: t.Name}
			defs = append(defs, contract.ToolDef{Name: name, Description: t.Description, InputSchema: schema, ReadOnly: t.Annotations.ReadOnlyHint})
		}
	}
	return defs, nil
}

// Dispatch calls the server tool behind a listed name.
func (m *Manager) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	m.mu.Lock()
	r, ok := m.routes[call.Name]
	m.mu.Unlock()
	if !ok {
		return &contract.ToolResult{CallID: call.ID, Content: fmt.Sprintf("Tool %s is not available.", call.Name), IsError: true}, nil
	}
	args := json.RawMessage(strings.TrimSpace(call.Args))
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	result, err := r.server.session.callTool(ctx, r.tool, args)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return &contract.ToolResult{CallID: call.ID, Content: err.Error(), IsError: true}, nil
	}
	return toResult(call.ID, result), nil
}

// toResult converts MCP content: text and embedded text resources become
// text, images become image parts, and structured content stands in when no
// text was returned.
func toResult(callID string, r *callResult) *contract.ToolResult {
	var texts []string
	var parts []contract.ContentPart
	hasImage := false
	for _, c := range r.Content {
		switch c.Type {
		case "text":
			texts = append(texts, c.Text)
			parts = append(parts, contract.ContentPart{Type: "text", Text: c.Text})
		case "image":
			hasImage = true
			parts = append(parts, contract.ContentPart{Type: "image", MediaType: c.MimeType, Data: c.Data})
		case "resource":
			if c.Resource != nil && c.Resource.Text != "" {
				texts = append(texts, c.Resource.Text)
				parts = append(parts, contract.ContentPart{Type: "text", Text: c.Resource.Text})
			}
		}
	}
	if len(texts) == 0 && len(r.StructuredContent) > 0 && string(r.StructuredContent) != "null" {
		texts = append(texts, string(r.StructuredContent))
		parts = append(parts, contract.ContentPart{Type: "text", Text: string(r.StructuredContent)})
	}
	result := &contract.ToolResult{CallID: callID, Content: strings.Join(texts, "\n"), IsError: r.IsError}
	if hasImage {
		result.Parts = parts
	}
	return result
}

// Close stops every server.
func (m *Manager) Close() {
	for _, s := range m.servers {
		_ = s.session.transport.close()
	}
}
