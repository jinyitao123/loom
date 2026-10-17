// Package interactive runs a long-lived, interactive Loom agent: a session
// file that survives restarts, a tool loop driven one prompt at a time, and a
// JSONL command/event protocol on stdio for the host application that embeds
// it. It is a Layer-3 host package: it composes stdlib and contract and never
// adds primitives to the kernel.
//
// The session file and the protocol follow the widely used coding-agent JSONL
// conventions (session version 3, tree-shaped entries, `message_update`
// streaming events), so hosts written for such agents can drive Loom
// unchanged.
//
// interactive 包承载长期运行的交互式 Loom 智能体：可跨重启续接的会话文件、
// 逐条提问驱动的工具循环，以及面向宿主的 stdio JSONL 命令/事件协议。
// 它属于第三层宿主：只组合 stdlib 与 contract，不向内核添加原语。
package interactive

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jinyitao123/loom/contract"
)

// Block is one typed content block of a message.
// Block 是消息中的一个内容块：文本、图片、思考或工具调用。
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Data      string          `json:"data,omitempty"`
	MimeType  string          `json:"mimeType,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Cost is a monetary breakdown in USD.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// Usage is the token and cost accounting of one model response.
type Usage struct {
	Input       int  `json:"input"`
	Output      int  `json:"output"`
	CacheRead   int  `json:"cacheRead"`
	CacheWrite  int  `json:"cacheWrite"`
	TotalTokens int  `json:"totalTokens"`
	Cost        Cost `json:"cost"`
}

// Message is one conversation message as stored in the session file and sent
// over the protocol. Role selects which fields are meaningful: "user",
// "assistant", "toolResult", "custom", "bashExecution", "compactionSummary",
// "branchSummary".
// Message 是会话文件与协议共用的消息形态；Role 决定哪些字段有意义。
type Message struct {
	Role         string          `json:"role"`
	Content      []Block         `json:"content,omitempty"`
	Timestamp    int64           `json:"timestamp"`
	API          string          `json:"api,omitempty"`
	Provider     string          `json:"provider,omitempty"`
	Model        string          `json:"model,omitempty"`
	Usage        *Usage          `json:"usage,omitempty"`
	StopReason   string          `json:"stopReason,omitempty"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
	ToolCallID   string          `json:"toolCallId,omitempty"`
	ToolName     string          `json:"toolName,omitempty"`
	IsError      *bool           `json:"isError,omitempty"`
	Details      json.RawMessage `json:"details,omitempty"`
	CustomType   string          `json:"customType,omitempty"`
	Display      *bool           `json:"display,omitempty"`
	Summary      string          `json:"summary,omitempty"`
	TokensBefore int             `json:"tokensBefore,omitempty"`
	FromID       string          `json:"fromId,omitempty"`
	Command      string          `json:"command,omitempty"`
	Output       string          `json:"output,omitempty"`
	ExitCode     *int            `json:"exitCode,omitempty"`
	Excluded     bool            `json:"excludeFromContext,omitempty"`
}

// UnmarshalJSON accepts content either as a plain string or as a block array.
// UnmarshalJSON 兼容两种 content 写法：纯字符串或内容块数组。
func (m *Message) UnmarshalJSON(data []byte) error {
	type alias Message
	var raw struct {
		alias
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*m = Message(raw.alias)
	m.Content = nil
	content := strings.TrimSpace(string(raw.Content))
	switch {
	case content == "" || content == "null":
	case strings.HasPrefix(content, "\""):
		var text string
		if err := json.Unmarshal(raw.Content, &text); err != nil {
			return fmt.Errorf("interactive: message content: %w", err)
		}
		m.Content = []Block{{Type: "text", Text: text}}
	default:
		if err := json.Unmarshal(raw.Content, &m.Content); err != nil {
			return fmt.Errorf("interactive: message content: %w", err)
		}
	}
	return nil
}

// Text joins the message's text blocks.
func (m Message) Text() string {
	var b strings.Builder
	for _, block := range m.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

func textBlocks(text string) []Block {
	if text == "" {
		return nil
	}
	return []Block{{Type: "text", Text: text}}
}

func boolPtr(v bool) *bool { return &v }

// toContract converts session messages into the provider-neutral history a
// tool loop consumes. Thinking blocks stay out of the request. With vision,
// user and tool messages that hold images carry them as Parts; without it an
// image becomes a short note. Every assistant tool call is guaranteed a
// matching tool message, so an aborted turn never leaves the provider with an
// unanswered call.
// toContract 把会话消息转换成工具循环使用的中性历史：思考块不进请求；
// 模型支持图片时图片以 Parts 送出，否则以说明文字代替；保证每个工具调用都有回执。
func toContract(messages []Message, vision bool) []contract.Message {
	out := make([]contract.Message, 0, len(messages))
	answered := map[string]bool{}
	for _, m := range messages {
		if m.Role == "toolResult" {
			answered[m.ToolCallID] = true
		}
	}
	for _, m := range messages {
		switch m.Role {
		case "user", "custom":
			msg := contract.Message{Role: "user", Content: contentText(m.Content, vision), Parts: contentParts(m.Content, vision)}
			if msg.Content != "" || len(msg.Parts) > 0 {
				out = append(out, msg)
			}
		case "assistant":
			msg := contract.Message{Role: "assistant", Content: m.Text()}
			var missing []string
			for _, block := range m.Content {
				if block.Type != "toolCall" {
					continue
				}
				args := string(block.Arguments)
				if args == "" {
					args = "{}"
				}
				msg.ToolCalls = append(msg.ToolCalls, contract.ToolCall{ID: block.ID, Name: block.Name, Args: args})
				if !answered[block.ID] {
					missing = append(missing, block.ID)
				}
			}
			if msg.Content == "" && len(msg.ToolCalls) == 0 {
				continue
			}
			out = append(out, msg)
			for _, id := range missing {
				out = append(out, contract.Message{Role: "tool", ToolCallID: id, Content: "Tool call was not completed."})
			}
		case "toolResult":
			out = append(out, contract.Message{Role: "tool", ToolCallID: m.ToolCallID, Content: contentText(m.Content, vision), Parts: contentParts(m.Content, vision)})
		case "bashExecution":
			if !m.Excluded {
				out = append(out, contract.Message{Role: "user", Content: fmt.Sprintf("Ran `%s`\n```\n%s\n```", m.Command, m.Output)})
			}
		case "compactionSummary":
			out = append(out, contract.Message{Role: "user", Content: "The conversation history before this point was compacted into the following summary:\n<summary>\n" + m.Summary + "\n</summary>"})
		case "branchSummary":
			out = append(out, contract.Message{Role: "user", Content: "The following is a summary of a branch that this conversation came back from:\n<summary>\n" + m.Summary + "\n</summary>"})
		}
	}
	return out
}

// contentText is the text rendering of content blocks. Images are omitted
// when they travel as parts and noted otherwise.
func contentText(blocks []Block, vision bool) string {
	var parts []string
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				parts = append(parts, block.Text)
			}
		case "image":
			if !vision {
				parts = append(parts, "[An image was attached here; the current model does not accept images.]")
			}
		}
	}
	return strings.Join(parts, "\n")
}

// contentParts returns text and image parts when blocks hold an image and the
// model accepts images; otherwise nil, so text-only messages stay unchanged.
func contentParts(blocks []Block, vision bool) []contract.ContentPart {
	if !vision {
		return nil
	}
	hasImage := false
	for _, block := range blocks {
		hasImage = hasImage || (block.Type == "image" && block.Data != "")
	}
	if !hasImage {
		return nil
	}
	parts := make([]contract.ContentPart, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				parts = append(parts, contract.ContentPart{Type: "text", Text: block.Text})
			}
		case "image":
			if block.Data != "" {
				parts = append(parts, contract.ContentPart{Type: "image", MediaType: block.MimeType, Data: block.Data})
			}
		}
	}
	return parts
}

// supportsImages reports whether a model accepts image input.
func supportsImages(m Model) bool {
	for _, input := range m.Input {
		if input == "image" {
			return true
		}
	}
	return false
}
