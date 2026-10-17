// Package mcpclient connects to Model Context Protocol servers over stdio or
// streamable HTTP and exposes their tools as a contract.ToolDispatcher. It
// answers server requests for user input (elicitation) through a callback the
// host supplies. It is a leaf adapter, like the providers: it implements
// contract interfaces and imports nothing above contract.
//
// mcpclient 通过 stdio 或 streamable HTTP 连接 MCP 服务器，把其工具作为
// contract.ToolDispatcher 提供；服务器请求用户输入（elicitation）时经宿主提供的回调作答。
// 与 provider 一样是叶子适配器，只实现 contract 接口。
package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// ProtocolVersion is the MCP revision this client speaks.
const ProtocolVersion = "2025-06-18"

// ElicitResult is the user's answer to an elicitation request.
type ElicitResult struct {
	Action  string         `json:"action"` // accept, decline or cancel
	Content map[string]any `json:"content,omitempty"`
}

// Elicitor asks the user for input on behalf of a server.
type Elicitor func(ctx context.Context, server, message string, schema json.RawMessage) (ElicitResult, error)

// rpcMessage is any JSON-RPC 2.0 message.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("MCP error %d: %s", e.Code, e.Message) }

// transport carries JSON-RPC messages; incoming messages go to the receive
// callback given when it was opened.
type transport interface {
	send(ctx context.Context, message []byte) error
	close() error
}

// session is one initialized connection to a server.
type session struct {
	name      string
	transport transport
	elicit    Elicitor
	nextID    atomic.Int64

	mu      sync.Mutex
	pending map[string]chan rpcMessage
	closed  bool

	instructions string
	serverName   string
	onListChange func()
}

func newSession(name string, elicit Elicitor) *session {
	return &session{name: name, elicit: elicit, pending: map[string]chan rpcMessage{}}
}

// receive handles one message from the server.
func (s *session) receive(raw []byte) {
	var msg rpcMessage
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	switch {
	case msg.Method != "" && len(msg.ID) > 0:
		go s.answer(msg)
	case msg.Method != "":
		if msg.Method == "notifications/tools/list_changed" && s.onListChange != nil {
			s.onListChange()
		}
	default:
		s.mu.Lock()
		ch := s.pending[string(msg.ID)]
		delete(s.pending, string(msg.ID))
		s.mu.Unlock()
		if ch != nil {
			ch <- msg
		}
	}
}

// answer replies to a request the server sent.
func (s *session) answer(req rpcMessage) {
	reply := rpcMessage{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "ping":
		reply.Result = json.RawMessage(`{}`)
	case "elicitation/create":
		var params struct {
			Message         string          `json:"message"`
			RequestedSchema json.RawMessage `json:"requestedSchema"`
		}
		_ = json.Unmarshal(req.Params, &params)
		result := ElicitResult{Action: "cancel"}
		if s.elicit != nil {
			if r, err := s.elicit(context.Background(), s.name, params.Message, params.RequestedSchema); err == nil {
				result = r
			}
		}
		reply.Result, _ = json.Marshal(result)
	default:
		reply.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	if line, err := json.Marshal(reply); err == nil {
		_ = s.transport.send(context.Background(), line)
	}
}

// call sends a request and waits for its response.
func (s *session) call(ctx context.Context, method string, params, result any) error {
	id := s.nextID.Add(1)
	idRaw := json.RawMessage(fmt.Sprint(id))
	paramsRaw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	line, err := json.Marshal(rpcMessage{JSONRPC: "2.0", ID: idRaw, Method: method, Params: paramsRaw})
	if err != nil {
		return err
	}
	ch := make(chan rpcMessage, 1)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("MCP connection is closed")
	}
	s.pending[string(idRaw)] = ch
	s.mu.Unlock()
	if err := s.transport.send(ctx, line); err != nil {
		s.forget(idRaw)
		return err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	case <-ctx.Done():
		s.forget(idRaw)
		cancel, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": id, "reason": "cancelled"}})
		_ = s.transport.send(context.Background(), cancel)
		return ctx.Err()
	}
}

func (s *session) forget(id json.RawMessage) {
	s.mu.Lock()
	delete(s.pending, string(id))
	s.mu.Unlock()
}

// fail ends every pending request, for example when the server exits.
func (s *session) fail(err error) {
	s.mu.Lock()
	pending := s.pending
	s.pending = map[string]chan rpcMessage{}
	s.closed = true
	s.mu.Unlock()
	for id, ch := range pending {
		ch <- rpcMessage{ID: json.RawMessage(id), Error: &rpcError{Code: -32000, Message: err.Error()}}
	}
}

func (s *session) notify(ctx context.Context, method string) error {
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	return s.transport.send(ctx, line)
}

// initialize performs the MCP handshake.
func (s *session) initialize(ctx context.Context, clientVersion string) error {
	var result struct {
		ServerInfo struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{"elicitation": map[string]any{}},
		"clientInfo":      map[string]any{"name": "loom", "version": clientVersion},
	}
	if err := s.call(ctx, "initialize", params, &result); err != nil {
		return err
	}
	s.instructions = result.Instructions
	s.serverName = result.ServerInfo.Name
	return s.notify(ctx, "notifications/initialized")
}

// tool is one tool a server offers.
type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations struct {
		ReadOnlyHint bool `json:"readOnlyHint"`
	} `json:"annotations"`
}

func (s *session) listTools(ctx context.Context) ([]tool, error) {
	var tools []tool
	cursor := ""
	for page := 0; page < 100; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var result struct {
			Tools      []tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := s.call(ctx, "tools/list", params, &result); err != nil {
			return nil, err
		}
		tools = append(tools, result.Tools...)
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	return tools, nil
}

// toolContent is one block of a tools/call result.
type toolContent struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
	Resource *struct {
		URI  string `json:"uri"`
		Text string `json:"text"`
	} `json:"resource"`
}

type callResult struct {
	Content           []toolContent   `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

func (s *session) callTool(ctx context.Context, name string, args json.RawMessage) (*callResult, error) {
	var result callResult
	err := s.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &result)
	return &result, err
}
