package stdlib

import (
	"context"
	"fmt"
	"strings"

	"github.com/jinyitao123/loom/contract"
)

type SummaryCompactionOpts struct {
	Model          string
	TokenThreshold int
	KeepLast       int
	Prompt         string
	Prefix         string
	PreCompact     func(context.Context, []contract.Message) error
}

// NewSummaryCompactionPolicy creates a complete trigger-and-summary policy for
// a ToolLoop. A failed summary leaves the original transcript intact.
func NewSummaryCompactionPolicy(llm contract.LLM, opts SummaryCompactionOpts) *CompactionPolicy {
	threshold := opts.TokenThreshold
	if threshold <= 0 {
		threshold = 6000
	}
	keepLast := opts.KeepLast
	if keepLast <= 0 {
		keepLast = 2
	}
	prompt := opts.Prompt
	if prompt == "" {
		prompt = "Summarize the following conversation into a concise paragraph. Preserve key facts, decisions, tool observations, and unfinished work. Output only the summary."
	}
	prefix := opts.Prefix
	if prefix == "" {
		prefix = "[Previous conversation summary]\n"
	}
	return &CompactionPolicy{
		Trigger:    func(_ []contract.Message, tokenCount int) bool { return tokenCount > threshold },
		PreCompact: opts.PreCompact,
		Compactor: func(ctx context.Context, messages []contract.Message) ([]contract.Message, error) {
			if llm == nil || len(messages) <= keepLast+2 {
				return messages, nil
			}
			start := 0
			var system *contract.Message
			if messages[0].Role == "system" {
				copy := messages[0]
				system = &copy
				start = 1
			}
			if len(messages)-start <= keepLast {
				return messages, nil
			}
			// A single assistant message can request several tools. Keeping a fixed
			// number of trailing messages could retain their results while
			// summarizing away the assistant's tool_calls. Move the cut to the
			// beginning of that whole exchange instead.
			cut := len(messages) - keepLast
			for cut > start && messages[cut].Role == "tool" {
				cut--
			}
			if cut == start {
				return messages, nil
			}
			var transcript strings.Builder
			for _, message := range messages[start:cut] {
				transcript.WriteString(fmt.Sprintf("[%s]: %s\n", message.Role, message.Content))
			}
			response, err := llm.Chat(ctx, contract.ChatRequest{
				Model: opts.Model,
				Messages: []contract.Message{
					{Role: "system", Content: prompt},
					{Role: "user", Content: transcript.String()},
				},
			})
			if err != nil || response == nil {
				return messages, nil
			}
			compacted := make([]contract.Message, 0, len(messages)-cut+2)
			if system != nil {
				compacted = append(compacted, *system)
			}
			compacted = append(compacted, contract.Message{Role: "assistant", Content: prefix + response.Content})
			compacted = append(compacted, messages[cut:]...)
			return compacted, nil
		},
	}
}
