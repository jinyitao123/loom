// Package deepseek implements the contract.LLM interface for the DeepSeek API.
// DeepSeek uses an OpenAI-compatible API at https://api.deepseek.com.
package deepseek

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jinyitao123/loom/contract"
)

// Client implements contract.LLM for DeepSeek.
type Client struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

// New creates a new DeepSeek client.
func New(apiKey string) *Client {
	return &Client{
		apiKey:  apiKey,
		baseURL: "https://api.deepseek.com",
		client:  http.DefaultClient,
	}
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
	Type     string         `json:"type"`
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

// Chat implements contract.LLM.
func (c *Client) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	// Convert messages.
	msgs := make([]oaiMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = oaiMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			msgs[i].ToolCalls = append(msgs[i].ToolCalls, oaiToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: tc.Name, Arguments: tc.Args},
			})
		}
	}

	// Convert tools.
	var tools []oaiTool
	for _, t := range req.Tools {
		tools = append(tools, oaiTool{
			Type: "function",
			Function: oaiToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	model := req.Model
	if model == "" {
		model = "deepseek-chat"
	}

	oaiReq := oaiRequest{
		Model:       model,
		Messages:    msgs,
		Tools:       tools,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}

	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("deepseek: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("deepseek: http: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("deepseek: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("deepseek: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var oaiResp oaiResponse
	if err := json.Unmarshal(respBody, &oaiResp); err != nil {
		return nil, fmt.Errorf("deepseek: unmarshal response: %w", err)
	}

	if oaiResp.Error != nil {
		return nil, fmt.Errorf("deepseek: API error: %s", oaiResp.Error.Message)
	}

	if len(oaiResp.Choices) == 0 {
		return nil, fmt.Errorf("deepseek: empty response")
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
	// Convert messages (same as Chat).
	msgs := make([]oaiMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = oaiMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			msgs[i].ToolCalls = append(msgs[i].ToolCalls, oaiToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: tc.Name, Arguments: tc.Args},
			})
		}
	}

	var tools []oaiTool
	for _, t := range req.Tools {
		tools = append(tools, oaiTool{
			Type: "function",
			Function: oaiToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	model := req.Model
	if model == "" {
		model = "deepseek-chat"
	}

	oaiReq := oaiRequest{
		Model:         model,
		Messages:      msgs,
		Tools:         tools,
		MaxTokens:     req.MaxTokens,
		Temperature:   req.Temperature,
		Stream:        true,
		StreamOptions: &oaiStreamOptions{IncludeUsage: true},
	}

	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("deepseek: marshal request: %w", err)
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
		return nil, fmt.Errorf("deepseek: http: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("deepseek: HTTP %d: %s", resp.StatusCode, string(errBody))
	}

	ch := make(chan contract.StreamChunk, 32)

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		// Accumulate tool calls by index across chunks.
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
				// Flush accumulated tool calls.
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

			// Capture usage from any chunk (DeepSeek sends it in a
			// dedicated chunk with empty choices when stream_options.include_usage=true).
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

			// Content delta.
			if delta.Content != "" {
				ch <- contract.StreamChunk{Content: delta.Content}
			}

			// Tool call deltas — accumulate by index.
			for _, tc := range delta.ToolCalls {
				idx := tc.Index
				if _, ok := toolCallMap[idx]; !ok {
					toolCallMap[idx] = &contract.ToolCall{
						ID:   tc.ID,
						Name: tc.Function.Name,
						Args: tc.Function.Arguments,
					}
				} else {
					// Append to existing (streaming args).
					toolCallMap[idx].Args += tc.Function.Arguments
				}
			}
		}
	}()

	return ch, nil
}
