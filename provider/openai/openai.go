// Package openai implements contract.LLM for any OpenAI-compatible API.
// Works with OpenAI, DeepSeek, OpenRouter, Together, Ollama, vLLM, Azure OpenAI, etc.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/anthropic/loom/contract"
)

// Client implements contract.LLM for any OpenAI-compatible endpoint.
type Client struct {
	apiKey       string
	baseURL      string
	defaultModel string
	client       *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL sets the API base URL (default: https://api.openai.com).
func WithBaseURL(url string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(url, "/") }
}

// WithDefaultModel sets the default model when none is specified in the request.
func WithDefaultModel(model string) Option {
	return func(c *Client) { c.defaultModel = model }
}

// New creates a new OpenAI-compatible client.
func New(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:       apiKey,
		baseURL:      "https://api.openai.com",
		defaultModel: "gpt-4o",
		client:       http.DefaultClient,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// openAI-compatible request/response types.

type oaiRequest struct {
	Model         string             `json:"model"`
	Messages      []oaiMessage       `json:"messages"`
	Tools         []oaiTool          `json:"tools,omitempty"`
	MaxTokens     int                `json:"max_tokens,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	StreamOptions *oaiStreamOptions  `json:"stream_options,omitempty"`
}

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaiTool struct {
	Type     string          `json:"type"`
	Function oaiToolFunction `json:"function"`
}

type oaiToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type oaiToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiResponse struct {
	Choices []struct {
		Message struct {
			Role      string        `json:"role"`
			Content   string        `json:"content"`
			ToolCalls []oaiToolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *Client) resolveModel(model string) string {
	if model != "" {
		return model
	}
	return c.defaultModel
}

func (c *Client) buildMessages(msgs []contract.Message) []oaiMessage {
	out := make([]oaiMessage, len(msgs))
	for i, m := range msgs {
		out[i] = oaiMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			out[i].ToolCalls = append(out[i].ToolCalls, oaiToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: tc.Name, Arguments: tc.Args},
			})
		}
	}
	return out
}

func (c *Client) buildTools(tools []contract.ToolDef) []oaiTool {
	var out []oaiTool
	for _, t := range tools {
		out = append(out, oaiTool{
			Type: "function",
			Function: oaiToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return out
}

// Chat implements contract.LLM.
func (c *Client) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	oaiReq := oaiRequest{
		Model:       c.resolveModel(req.Model),
		Messages:    c.buildMessages(req.Messages),
		Tools:       c.buildTools(req.Tools),
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}

	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: http: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var oaiResp oaiResponse
	if err := json.Unmarshal(respBody, &oaiResp); err != nil {
		return nil, fmt.Errorf("openai: unmarshal response: %w", err)
	}

	if oaiResp.Error != nil {
		return nil, fmt.Errorf("openai: API error: %s", oaiResp.Error.Message)
	}

	if len(oaiResp.Choices) == 0 {
		return nil, fmt.Errorf("openai: empty response")
	}

	choice := oaiResp.Choices[0]
	chatResp := &contract.ChatResponse{
		Content:    choice.Message.Content,
		StopReason: choice.FinishReason,
		Usage: contract.Usage{
			InputTokens:  oaiResp.Usage.PromptTokens,
			OutputTokens: oaiResp.Usage.CompletionTokens,
		},
	}

	for _, tc := range choice.Message.ToolCalls {
		chatResp.ToolCalls = append(chatResp.ToolCalls, contract.ToolCall{
			ID:   tc.ID,
			Name: tc.Function.Name,
			Args: tc.Function.Arguments,
		})
	}

	return chatResp, nil
}

// oaiStreamChunk is one SSE chunk from the streaming API.
type oaiStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string        `json:"content,omitempty"`
			ToolCalls []oaiToolCall `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage,omitempty"`
}

// Stream implements contract.LLM with SSE streaming.
func (c *Client) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	oaiReq := oaiRequest{
		Model:         c.resolveModel(req.Model),
		Messages:      c.buildMessages(req.Messages),
		Tools:         c.buildTools(req.Tools),
		MaxTokens:     req.MaxTokens,
		Temperature:   req.Temperature,
		Stream:        true,
		StreamOptions: &oaiStreamOptions{IncludeUsage: true},
	}

	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: http: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, string(errBody))
	}

	ch := make(chan contract.StreamChunk, 32)

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		toolCallMap := make(map[int]*contract.ToolCall)
		var lastUsage *contract.Usage

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()

			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				if len(toolCallMap) > 0 {
					var tcs []contract.ToolCall
					for i := 0; i < len(toolCallMap); i++ {
						if tc, ok := toolCallMap[i]; ok {
							tcs = append(tcs, *tc)
						}
					}
					ch <- contract.StreamChunk{ToolCalls: tcs, Done: true, Usage: lastUsage}
				} else {
					ch <- contract.StreamChunk{Done: true, Usage: lastUsage}
				}
				return
			}

			var chunk oaiStreamChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}

			// Capture usage from any chunk (sent in a dedicated chunk
			// with empty choices when stream_options.include_usage=true).
			if chunk.Usage != nil {
				lastUsage = &contract.Usage{
					InputTokens:  chunk.Usage.PromptTokens,
					OutputTokens: chunk.Usage.CompletionTokens,
				}
			}

			if len(chunk.Choices) == 0 {
				continue
			}

			delta := chunk.Choices[0].Delta

			if delta.Content != "" {
				ch <- contract.StreamChunk{Content: delta.Content}
			}

			for _, tc := range delta.ToolCalls {
				idx := tc.Index
				if _, ok := toolCallMap[idx]; !ok {
					toolCallMap[idx] = &contract.ToolCall{
						ID:   tc.ID,
						Name: tc.Function.Name,
						Args: tc.Function.Arguments,
					}
				} else {
					toolCallMap[idx].Args += tc.Function.Arguments
				}
			}
		}
	}()

	return ch, nil
}

// Compile-time interface check.
var _ contract.LLM = (*Client)(nil)
