// Package anthropic implements contract.LLM for the Anthropic Messages API.
// It streams text, extended thinking and tool calls, sends signed thinking
// blocks back so the model keeps its reasoning across tool rounds, marks the
// system prompt, tools and newest message for prompt caching, and reports
// cache reads and writes.
//
// anthropic 包为 Anthropic Messages API 实现 contract.LLM：流式输出文本、扩展思考与工具调用；
// 回传带签名的思考块，使模型在工具轮次间保留推理；为系统提示、工具与最新消息标记提示缓存，
// 并上报缓存读写用量。它是叶子适配器，只依赖 contract。
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/internal/requestcheck"
)

// DefaultBaseURL is the public API endpoint; requests go to <base>/v1/messages.
const DefaultBaseURL = "https://api.anthropic.com"

// APIVersion is the anthropic-version header sent with every request.
const APIVersion = "2023-06-01"

// DefaultMaxTokens is the output limit when a request sets none.
const DefaultMaxTokens = 32000

// DefaultHeaderTimeout bounds the wait for response headers. The body of a
// stream may take much longer and is bounded only by the caller's context.
const DefaultHeaderTimeout = 120 * time.Second

// ThinkingBudgets maps contract effort levels to thinking token budgets.
var ThinkingBudgets = map[contract.EffortLevel]int{
	contract.EffortLow:    2048,
	contract.EffortMedium: 8192,
	contract.EffortHigh:   16384,
}

// ErrEmptyResponse reports a response with neither text nor tool calls.
var ErrEmptyResponse = errors.New("anthropic: empty response")

// Client calls the Messages API.
type Client struct {
	apiKey       string
	baseURL      string
	defaultModel string
	headers      map[string]string
	http         *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the client at another endpoint (a proxy or gateway).
func WithBaseURL(url string) Option {
	return func(c *Client) {
		if url != "" {
			c.baseURL = strings.TrimSuffix(strings.TrimSuffix(url, "/"), "/v1")
		}
	}
}

// WithDefaultModel sets the model used when a request names none.
func WithDefaultModel(model string) Option { return func(c *Client) { c.defaultModel = model } }

// WithHeader adds a header to every request, for example a beta flag.
func WithHeader(name, value string) Option {
	return func(c *Client) { c.headers[name] = value }
}

// WithHeaderTimeout replaces DefaultHeaderTimeout.
func WithHeaderTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.http.Transport.(*http.Transport).ResponseHeaderTimeout = d
		}
	}
}

// WithAttemptTimeout bounds one complete physical request, including its stream.
// A non-positive duration leaves the caller-context-only default unchanged.
func WithAttemptTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.http.Timeout = d
		}
	}
}

// New returns a client authenticated with apiKey.
func New(apiKey string, opts ...Option) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = DefaultHeaderTimeout
	c := &Client{apiKey: apiKey, baseURL: DefaultBaseURL, headers: map[string]string{}, http: &http.Client{Transport: transport}}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ---- request ----

type request struct {
	Model        string        `json:"model"`
	MaxTokens    int           `json:"max_tokens"`
	System       []block       `json:"system,omitempty"`
	Messages     []message     `json:"messages"`
	Tools        []tool        `json:"tools,omitempty"`
	ToolChoice   *toolChoice   `json:"tool_choice,omitempty"`
	Temperature  *float64      `json:"temperature,omitempty"`
	Thinking     *thinking     `json:"thinking,omitempty"`
	Stream       bool          `json:"stream"`
	OutputConfig *outputConfig `json:"output_config,omitempty"`
}

type outputConfig struct {
	Format outputFormat `json:"format"`
}
type outputFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema"`
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

// block is any content block; only the fields of its type are set.
type block struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Source       *imageSource    `json:"source,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      []block         `json:"content,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	Signature    string          `json:"signature,omitempty"`
	Data         string          `json:"data,omitempty"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type cacheControl struct {
	Type string `json:"type"`
}

type tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

type toolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type thinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

var ephemeral = &cacheControl{Type: "ephemeral"}

// buildRequest converts a contract request to the wire form.
func (c *Client) buildRequest(req contract.ChatRequest) (request, error) {
	if err := requestcheck.Validate(req); err != nil {
		return request{}, fmt.Errorf("anthropic: %w", err)
	}
	model := req.Model
	if model == "" {
		model = c.defaultModel
	}
	if model == "" {
		return request{}, errors.New("anthropic: no model")
	}
	out := request{Model: model, MaxTokens: req.MaxTokens, Temperature: req.Temperature, Stream: true}
	if out.MaxTokens <= 0 {
		out.MaxTokens = DefaultMaxTokens
	}
	if req.Schema != nil {
		out.OutputConfig = &outputConfig{Format: outputFormat{Type: "json_schema", Schema: *req.Schema}}
	}
	var system []string
	for _, m := range req.Messages {
		if m.Role == "system" {
			if m.Content != "" {
				system = append(system, m.Content)
			}
			continue
		}
		blocks := convertMessage(m)
		if len(blocks) == 0 {
			continue
		}
		role := "user"
		if m.Role == "assistant" {
			role = "assistant"
		}
		// Consecutive messages of one role (several tool results, or tool
		// results followed by user input) form one turn.
		if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == role {
			out.Messages[n-1].Content = append(out.Messages[n-1].Content, blocks...)
		} else {
			out.Messages = append(out.Messages, message{Role: role, Content: blocks})
		}
	}
	if len(out.Messages) == 0 {
		return request{}, errors.New("anthropic: no messages")
	}
	if len(system) > 0 {
		out.System = []block{{Type: "text", Text: strings.Join(system, "\n\n"), CacheControl: ephemeral}}
	}
	for _, def := range req.Tools {
		schema := def.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out.Tools = append(out.Tools, tool{Name: def.Name, Description: def.Description, InputSchema: schema})
	}
	if n := len(out.Tools); n > 0 {
		out.Tools[n-1].CacheControl = ephemeral
	}
	// The newest message closes the cached prefix, so the next request reads
	// everything before it from the cache.
	last := &out.Messages[len(out.Messages)-1]
	last.Content[len(last.Content)-1].CacheControl = ephemeral

	if req.ToolChoice != nil && len(out.Tools) > 0 {
		switch req.ToolChoice.Mode {
		case contract.ToolChoiceAuto:
			out.ToolChoice = &toolChoice{Type: "auto"}
		case contract.ToolChoiceNone:
			out.ToolChoice = &toolChoice{Type: "none"}
		case contract.ToolChoiceRequired:
			out.ToolChoice = &toolChoice{Type: "any"}
		case contract.ToolChoiceTool:
			out.ToolChoice = &toolChoice{Type: "tool", Name: req.ToolChoice.Name}
		}
	}
	if budget, ok := ThinkingBudgets[req.Effort]; ok {
		// The budget must stay below the output limit; thinking also rules
		// out a custom temperature and forced tool use.
		if budget >= out.MaxTokens {
			budget = out.MaxTokens / 2
		}
		forced := out.ToolChoice != nil && (out.ToolChoice.Type == "any" || out.ToolChoice.Type == "tool")
		if forced {
			return request{}, errors.New("anthropic: forced tool choice is incompatible with thinking")
		}
		if budget < 1024 {
			return request{}, errors.New("anthropic: max tokens too small for thinking")
		}
		if budget >= 1024 {
			out.Thinking = &thinking{Type: "enabled", BudgetTokens: budget}
			out.Temperature = nil
		}
	}
	return out, nil
}

// convertMessage returns the content blocks of one contract message.
func convertMessage(m contract.Message) []block {
	switch m.Role {
	case "assistant":
		var blocks []block
		for _, r := range m.Reasoning {
			// Only signed blocks can go back; reasoning from another provider
			// has no signature and is dropped.
			switch {
			case r.Redacted && r.Signature != "":
				blocks = append(blocks, block{Type: "redacted_thinking", Data: r.Signature})
			case r.Signature != "":
				blocks = append(blocks, block{Type: "thinking", Thinking: r.Text, Signature: r.Signature})
			}
		}
		if strings.TrimSpace(m.Content) != "" {
			blocks = append(blocks, block{Type: "text", Text: m.Content})
		}
		for _, call := range m.ToolCalls {
			input := json.RawMessage(call.Args)
			if strings.TrimSpace(call.Args) == "" || !json.Valid(input) {
				input = json.RawMessage(`{}`)
			}
			blocks = append(blocks, block{Type: "tool_use", ID: call.ID, Name: call.Name, Input: input})
		}
		return blocks
	case "tool":
		content := contentBlocks(m)
		if len(content) == 0 {
			content = []block{{Type: "text", Text: "(no output)"}}
		}
		return []block{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: content}}
	default:
		return contentBlocks(m)
	}
}

// contentBlocks renders text and images; empty text parts are left out, as
// the API rejects them.
func contentBlocks(m contract.Message) []block {
	if len(m.Parts) == 0 {
		if strings.TrimSpace(m.Content) == "" {
			return nil
		}
		return []block{{Type: "text", Text: m.Content}}
	}
	var blocks []block
	for _, p := range m.Parts {
		switch p.Type {
		case "text":
			if strings.TrimSpace(p.Text) != "" {
				blocks = append(blocks, block{Type: "text", Text: p.Text})
			}
		case "image":
			blocks = append(blocks, block{Type: "image", Source: &imageSource{Type: "base64", MediaType: p.MediaType, Data: p.Data}})
		}
	}
	return blocks
}

// ---- response ----

// event is one server-sent event of the stream.
type event struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage usage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		Text string `json:"text"`
		Data string `json:"data"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *usage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type usage struct {
	InputTokens              int  `json:"input_tokens"`
	OutputTokens             int  `json:"output_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
}

// openBlock accumulates one content block while it streams.
type openBlock struct {
	kind      string
	text      strings.Builder
	signature string
	data      string
	call      contract.ToolCall
	args      strings.Builder
}

// stopReason maps the API's stop reasons to contract ones.
func stopReason(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "end_turn", "stop_sequence":
		return "stop"
	default:
		return reason
	}
}

// Stream sends the request and streams its response. Text and thinking
// arrive as deltas; each completed thinking block follows as a
// ReasoningBlock; tool calls and usage come with the final chunk.
func (c *Client) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	wire, err := c.buildRequest(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("anthropic-version", APIVersion)
	if c.apiKey != "" {
		httpReq.Header.Set("x-api-key", c.apiKey)
	}
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, fmt.Errorf("anthropic: timeout waiting for response: %w", err)
		}
		return nil, fmt.Errorf("anthropic: http: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, apiError(resp.StatusCode, data)
	}
	ch := make(chan contract.StreamChunk, 64)
	go c.read(ctx, resp.Body, ch)
	return ch, nil
}

// apiError formats an error response as "HTTP <status>: <type>: <message>".
func apiError(status int, data []byte) error {
	var parsed struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &parsed) == nil && parsed.Error.Message != "" {
		return fmt.Errorf("anthropic: HTTP %d: %s: %s", status, parsed.Error.Type, parsed.Error.Message)
	}
	return fmt.Errorf("anthropic: HTTP %d: %s", status, strings.TrimSpace(string(data)))
}

func (c *Client) read(ctx context.Context, body io.ReadCloser, ch chan<- contract.StreamChunk) {
	defer close(ch)
	defer body.Close()
	send := func(chunk contract.StreamChunk) bool {
		select {
		case ch <- chunk:
			return true
		case <-ctx.Done():
			return false
		}
	}
	fail := func(err error) { send(contract.StreamChunk{Err: err}) }

	blocks := map[int]*openBlock{}
	calls := map[int]contract.ToolCall{}
	var total usage
	var reason string
	sawOutput := false

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			fail(fmt.Errorf("anthropic: invalid stream event: %w", err))
			return
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				total = ev.Message.Usage
			}
		case "content_block_start":
			if ev.ContentBlock == nil {
				continue
			}
			b := &openBlock{kind: ev.ContentBlock.Type, data: ev.ContentBlock.Data}
			if b.kind == "tool_use" {
				b.call = contract.ToolCall{ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}
			}
			if ev.ContentBlock.Text != "" {
				b.text.WriteString(ev.ContentBlock.Text)
				sawOutput = true
				if !send(contract.StreamChunk{Content: ev.ContentBlock.Text}) {
					return
				}
			}
			blocks[ev.Index] = b
		case "content_block_delta":
			b := blocks[ev.Index]
			if b == nil || ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					sawOutput = true
					if !send(contract.StreamChunk{Content: ev.Delta.Text}) {
						return
					}
				}
			case "thinking_delta":
				b.text.WriteString(ev.Delta.Thinking)
				if ev.Delta.Thinking != "" && !send(contract.StreamChunk{Reasoning: ev.Delta.Thinking}) {
					return
				}
			case "signature_delta":
				b.signature += ev.Delta.Signature
			case "input_json_delta":
				b.args.WriteString(ev.Delta.PartialJSON)
			}
		case "content_block_stop":
			b := blocks[ev.Index]
			if b == nil {
				continue
			}
			delete(blocks, ev.Index)
			switch b.kind {
			case "thinking":
				if !send(contract.StreamChunk{ReasoningBlock: &contract.Reasoning{Text: b.text.String(), Signature: b.signature}}) {
					return
				}
			case "redacted_thinking":
				if !send(contract.StreamChunk{ReasoningBlock: &contract.Reasoning{Signature: b.data, Redacted: true}}) {
					return
				}
			case "tool_use":
				b.call.Args = b.args.String()
				if strings.TrimSpace(b.call.Args) == "" {
					b.call.Args = "{}"
				}
				calls[ev.Index] = b.call
				sawOutput = true
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				reason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				// message_delta carries cumulative output; the input figures
				// repeat message_start's when present.
				total.OutputTokens = ev.Usage.OutputTokens
				if ev.Usage.InputTokens > 0 {
					total.InputTokens = ev.Usage.InputTokens
				}
				if ev.Usage.CacheReadInputTokens != nil {
					total.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
				}
				if ev.Usage.CacheCreationInputTokens != nil {
					total.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens
				}
			}
		case "message_stop":
			if len(blocks) != 0 || reason == "" {
				fail(errors.New("anthropic: incomplete message before message_stop"))
				return
			}
			if !sawOutput {
				fail(fmt.Errorf("%w (stop reason %q)", ErrEmptyResponse, reason))
				return
			}
			u := contract.Usage{
				InputTokens:        total.InputTokens + tokenCount(total.CacheReadInputTokens) + tokenCount(total.CacheCreationInputTokens),
				OutputTokens:       total.OutputTokens,
				CacheReadTokens:    tokenCount(total.CacheReadInputTokens),
				CacheReadReported:  total.CacheReadInputTokens != nil,
				CacheWriteTokens:   tokenCount(total.CacheCreationInputTokens),
				CacheWriteReported: total.CacheCreationInputTokens != nil,
			}
			send(contract.StreamChunk{ToolCalls: orderedCalls(calls), Done: true, Usage: &u, FinishReason: stopReason(reason)})
			return
		case "error":
			if ev.Error != nil {
				fail(fmt.Errorf("anthropic: stream error: %s: %s", ev.Error.Type, ev.Error.Message))
			} else {
				fail(errors.New("anthropic: stream error"))
			}
			return
		}
	}
	switch {
	case ctx.Err() != nil:
		fail(ctx.Err())
	case scanner.Err() != nil:
		fail(fmt.Errorf("anthropic: stream scanner: %w", scanner.Err()))
	default:
		fail(errors.New("anthropic: stream closed before message_stop (unexpected EOF)"))
	}
}

// Chat runs the request as a stream and assembles the response.
func (c *Client) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	ch, err := c.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	var resp contract.ChatResponse
	var text strings.Builder
	for chunk := range ch {
		if chunk.Err != nil {
			return nil, chunk.Err
		}
		text.WriteString(chunk.Content)
		if chunk.ReasoningBlock != nil {
			resp.Reasoning = append(resp.Reasoning, *chunk.ReasoningBlock)
		}
		if chunk.Done {
			resp.ToolCalls = chunk.ToolCalls
			if chunk.Usage != nil {
				resp.Usage = *chunk.Usage
			}
			resp.StopReason = chunk.FinishReason
			resp.Content = text.String()
			return &resp, nil
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, errors.New("anthropic: stream closed without a result")
}

var _ contract.LLM = (*Client)(nil)

// orderedCalls preserves provider output order even if blocks finish out of order.
func orderedCalls(calls map[int]contract.ToolCall) []contract.ToolCall {
	indices := make([]int, 0, len(calls))
	for i := range calls {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	var out []contract.ToolCall
	for _, i := range indices {
		out = append(out, calls[i])
	}
	return out
}

func tokenCount(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}
