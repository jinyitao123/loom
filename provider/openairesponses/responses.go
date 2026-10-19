// Package openairesponses implements contract.LLM for the OpenAI Responses
// API. Requests are stateless (store=false): the whole conversation is sent
// each time, and reasoning continues across tool rounds through encrypted
// reasoning items, carried as the signature of a contract reasoning block
// whose text is the reasoning summary.
//
// openairesponses 包为 OpenAI Responses API 实现 contract.LLM。请求不在服务端留存（store=false），
// 每次发送完整对话；推理经加密推理项在工具轮次间延续，加密内容作为推理块的签名，推理摘要作为其文本。
package openairesponses

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/internal/requestcheck"
)

// DefaultBaseURL is the public API base; requests go to <base>/responses.
const DefaultBaseURL = "https://api.openai.com/v1"

// DefaultHeaderTimeout bounds the wait for response headers. The stream body
// is bounded only by the caller's context.
const DefaultHeaderTimeout = 120 * time.Second

// ErrEmptyResponse reports a response with neither text nor tool calls.
var ErrEmptyResponse = errors.New("openai responses: empty response")

// Client calls the Responses API.
type Client struct {
	apiKey       string
	baseURL      string
	defaultModel string
	http         *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the client at another endpoint; it includes the version
// path, for example https://api.openai.com/v1.
func WithBaseURL(url string) Option {
	return func(c *Client) {
		if url != "" {
			c.baseURL = strings.TrimSuffix(url, "/")
		}
	}
}

// WithDefaultModel sets the model used when a request names none.
func WithDefaultModel(model string) Option { return func(c *Client) { c.defaultModel = model } }

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
	c := &Client{apiKey: apiKey, baseURL: DefaultBaseURL, http: &http.Client{Transport: transport}}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ---- request ----

type request struct {
	Model           string      `json:"model"`
	Instructions    string      `json:"instructions,omitempty"`
	Input           []any       `json:"input"`
	Tools           []tool      `json:"tools,omitempty"`
	ToolChoice      any         `json:"tool_choice,omitempty"`
	MaxOutputTokens int         `json:"max_output_tokens,omitempty"`
	Temperature     *float64    `json:"temperature,omitempty"`
	Reasoning       *reasoning  `json:"reasoning,omitempty"`
	Include         []string    `json:"include,omitempty"`
	Store           bool        `json:"store"`
	Stream          bool        `json:"stream"`
	Text            *textConfig `json:"text,omitempty"`
}

type textConfig struct {
	Format outputFormat `json:"format"`
}
type outputFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type reasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary"`
}

type part struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type inputMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type reasoningItem struct {
	Type             string `json:"type"`
	Summary          []part `json:"summary"`
	EncryptedContent string `json:"encrypted_content"`
}

type functionCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type functionOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output any    `json:"output"`
}

func (c *Client) buildRequest(req contract.ChatRequest) (request, error) {
	if err := requestcheck.Validate(req); err != nil {
		return request{}, fmt.Errorf("openairesponses: %w", err)
	}
	model := req.Model
	if model == "" {
		model = c.defaultModel
	}
	if model == "" {
		return request{}, errors.New("openai responses: no model")
	}
	out := request{Model: model, MaxOutputTokens: req.MaxTokens, Temperature: req.Temperature, Stream: true}
	if req.Schema != nil {
		out.Text = &textConfig{Format: outputFormat{Type: "json_schema", Name: "output", Strict: true, Schema: *req.Schema}}
	}
	var system []string
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if m.Content != "" {
				system = append(system, m.Content)
			}
		case "assistant":
			for _, r := range m.Reasoning {
				// Only encrypted reasoning from this API can go back.
				if r.Signature == "" || r.Redacted {
					continue
				}
				item := reasoningItem{Type: "reasoning", Summary: []part{}, EncryptedContent: r.Signature}
				if r.Text != "" {
					item.Summary = []part{{Type: "summary_text", Text: r.Text}}
				}
				out.Input = append(out.Input, item)
			}
			if strings.TrimSpace(m.Content) != "" {
				out.Input = append(out.Input, inputMessage{Role: "assistant", Content: m.Content})
			}
			for _, call := range m.ToolCalls {
				args := call.Args
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				out.Input = append(out.Input, functionCall{Type: "function_call", CallID: call.ID, Name: call.Name, Arguments: args})
			}
		case "tool":
			var output any = m.Content
			if hasImage(m.Parts) {
				output = parts(m)
			} else if m.Content == "" {
				output = "(no output)"
			}
			out.Input = append(out.Input, functionOutput{Type: "function_call_output", CallID: m.ToolCallID, Output: output})
		default:
			content := parts(m)
			if len(content) > 0 {
				out.Input = append(out.Input, inputMessage{Role: "user", Content: content})
			}
		}
	}
	if len(out.Input) == 0 {
		return request{}, errors.New("openai responses: no input")
	}
	out.Instructions = strings.Join(system, "\n\n")
	for _, def := range req.Tools {
		schema := def.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, tool{Type: "function", Name: def.Name, Description: def.Description, Parameters: schema})
	}
	if req.ToolChoice != nil && len(out.Tools) > 0 {
		switch req.ToolChoice.Mode {
		case contract.ToolChoiceAuto:
			out.ToolChoice = "auto"
		case contract.ToolChoiceNone:
			out.ToolChoice = "none"
		case contract.ToolChoiceRequired:
			out.ToolChoice = "required"
		case contract.ToolChoiceTool:
			out.ToolChoice = map[string]string{"type": "function", "name": req.ToolChoice.Name}
		}
	}
	switch req.Effort {
	case contract.EffortLow, contract.EffortMedium, contract.EffortHigh:
		// Reasoning models reject a custom temperature.
		out.Reasoning = &reasoning{Effort: string(req.Effort), Summary: "auto"}
		out.Include = []string{"reasoning.encrypted_content"}
		out.Temperature = nil
	}
	return out, nil
}

func hasImage(ps []contract.ContentPart) bool {
	for _, p := range ps {
		if p.Type == "image" {
			return true
		}
	}
	return false
}

// parts renders a message's text and images as input parts.
func parts(m contract.Message) []part {
	if len(m.Parts) == 0 {
		if strings.TrimSpace(m.Content) == "" {
			return nil
		}
		return []part{{Type: "input_text", Text: m.Content}}
	}
	var out []part
	for _, p := range m.Parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, part{Type: "input_text", Text: p.Text})
			}
		case "image":
			out = append(out, part{Type: "input_image", ImageURL: "data:" + p.MediaType + ";base64," + p.Data})
		}
	}
	return out
}

// ---- response ----

type event struct {
	Type        string          `json:"type"`
	OutputIndex int             `json:"output_index"`
	Delta       string          `json:"delta"`
	Item        *outputItem     `json:"item"`
	Response    *responseObject `json:"response"`
	Code        string          `json:"code"`
	Message     string          `json:"message"`
}

type outputItem struct {
	Type             string `json:"type"`
	CallID           string `json:"call_id"`
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	EncryptedContent string `json:"encrypted_content"`
	Summary          []part `json:"summary"`
}

type responseObject struct {
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Usage *struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens *int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
}

// Stream sends the request and streams its response: text and reasoning
// summaries as deltas, each completed reasoning item as a ReasoningBlock,
// and tool calls with the final chunk.
func (c *Client) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	wire, err := c.buildRequest(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("openai responses: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai responses: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai responses: http: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, apiError(resp.StatusCode, data)
	}
	ch := make(chan contract.StreamChunk, 64)
	go read(ctx, resp.Body, ch)
	return ch, nil
}

func apiError(status int, data []byte) error {
	var parsed struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &parsed) == nil && parsed.Error.Message != "" {
		kind := parsed.Error.Code
		if kind == "" {
			kind = parsed.Error.Type
		}
		return fmt.Errorf("openai responses: HTTP %d: %s: %s", status, kind, parsed.Error.Message)
	}
	return fmt.Errorf("openai responses: HTTP %d: %s", status, strings.TrimSpace(string(data)))
}

func read(ctx context.Context, body io.ReadCloser, ch chan<- contract.StreamChunk) {
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

	calls := map[int]contract.ToolCall{}
	sawText := false
	summaryStarted := map[int]bool{} // reasoning items whose summary already streamed text

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			fail(fmt.Errorf("openairesponses: invalid stream event: %w", err))
			return
		}
		switch ev.Type {
		case "response.output_text.delta":
			if ev.Delta != "" {
				sawText = true
				if !send(contract.StreamChunk{Content: ev.Delta}) {
					return
				}
			}
		case "response.reasoning_summary_text.delta":
			if ev.Delta != "" {
				summaryStarted[ev.OutputIndex] = true
				if !send(contract.StreamChunk{Reasoning: ev.Delta}) {
					return
				}
			}
		case "response.reasoning_summary_part.added":
			// Parts of one summary are separate paragraphs.
			if summaryStarted[ev.OutputIndex] && !send(contract.StreamChunk{Reasoning: "\n\n"}) {
				return
			}
		case "response.output_item.done":
			if ev.Item == nil {
				continue
			}
			switch ev.Item.Type {
			case "reasoning":
				var texts []string
				for _, p := range ev.Item.Summary {
					if p.Text != "" {
						texts = append(texts, p.Text)
					}
				}
				block := contract.Reasoning{Text: strings.Join(texts, "\n\n"), Signature: ev.Item.EncryptedContent}
				if block.Text != "" || block.Signature != "" {
					if !send(contract.StreamChunk{ReasoningBlock: &block}) {
						return
					}
				}
			case "function_call":
				args := ev.Item.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				calls[ev.OutputIndex] = contract.ToolCall{ID: ev.Item.CallID, Name: ev.Item.Name, Args: args}
			}
		case "response.completed", "response.incomplete":
			r := ev.Response
			if r == nil {
				fail(errors.New("openai responses: completion without a response"))
				return
			}
			if !sawText && len(calls) == 0 {
				reason := r.Status
				if r.IncompleteDetails != nil {
					reason = r.IncompleteDetails.Reason
				}
				fail(fmt.Errorf("%w (%s)", ErrEmptyResponse, reason))
				return
			}
			finish := "stop"
			switch {
			case ev.Type == "response.incomplete":
				if r.IncompleteDetails == nil || r.IncompleteDetails.Reason != "max_output_tokens" {
					fail(errors.New("openai responses: response incomplete"))
					return
				}
				finish = "length"
			case len(calls) > 0:
				finish = "tool_calls"
			}
			var usage *contract.Usage
			if r.Usage != nil {
				usage = &contract.Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens}
				if n := r.Usage.InputTokensDetails.CachedTokens; n != nil {
					usage.CacheReadTokens = *n
					usage.CacheReadReported = true
				}
			}
			send(contract.StreamChunk{ToolCalls: orderedCalls(calls), Done: true, Usage: usage, FinishReason: finish})
			return
		case "response.failed":
			if ev.Response != nil && ev.Response.Error != nil {
				fail(fmt.Errorf("openai responses: %s: %s", ev.Response.Error.Code, ev.Response.Error.Message))
			} else {
				fail(errors.New("openai responses: response failed"))
			}
			return
		case "error":
			fail(fmt.Errorf("openai responses: stream error: %s: %s", ev.Code, ev.Message))
			return
		}
	}
	switch {
	case ctx.Err() != nil:
		fail(ctx.Err())
	case scanner.Err() != nil:
		fail(fmt.Errorf("openai responses: stream scanner: %w", scanner.Err()))
	default:
		fail(errors.New("openai responses: stream closed before completion (unexpected EOF)"))
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
			resp.Content = text.String()
			resp.ToolCalls = chunk.ToolCalls
			if chunk.Usage != nil {
				resp.Usage = *chunk.Usage
			}
			resp.StopReason = chunk.FinishReason
			return &resp, nil
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, errors.New("openai responses: stream closed without a result")
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
