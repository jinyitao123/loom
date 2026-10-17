package interactive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jinyitao123/loom/contract"
)

// Compaction defaults, matching the common coding-agent settings.
const (
	DefaultReserveTokens    = 16384 // room left for the model's answer
	DefaultKeepRecentTokens = 20000 // recent history kept verbatim
	maxToolResultLen        = 2000  // tool result characters sent to the summarizer
	imageTokens             = 1200  // rough cost of one image
)

// ErrNothingToCompact reports a manual compaction with no history before the
// kept recent part.
var ErrNothingToCompact = errors.New("nothing to compact yet")

const summarySystemPrompt = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI coding assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

const summaryFormat = `Create a structured context checkpoint summary that another LLM will use to continue the work. Use this EXACT format:

## Goal
[What the user is trying to accomplish]

## Constraints & Preferences
- [Requirements and preferences the user mentioned]

## Progress
### Done
- [x] [Completed tasks]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues, if any]

## Key Decisions
- **[Decision]**: [Rationale]

## Next Steps
1. [What should happen next]

## Critical Context
- [Data, file contents, error messages or values needed to continue]

Keep each section concise. Preserve exact file paths, function names and error messages.`

// CompactionResult describes one completed compaction.
type CompactionResult struct {
	Summary              string          `json:"summary"`
	FirstKeptEntryID     string          `json:"firstKeptEntryId"`
	TokensBefore         int             `json:"tokensBefore"`
	EstimatedTokensAfter int             `json:"estimatedTokensAfter"`
	Usage                *Usage          `json:"usage,omitempty"`
	Details              json.RawMessage `json:"details,omitempty"`
}

type fileDetails struct {
	ReadFiles     []string `json:"readFiles"`
	ModifiedFiles []string `json:"modifiedFiles"`
}

// estimateTokens roughly counts the tokens of one message.
func estimateTokens(m Message) int {
	chars := len(m.Summary) + len(m.Command) + len(m.Output) + len(m.ErrorMessage)
	tokens := 0
	for _, b := range m.Content {
		switch b.Type {
		case "image":
			tokens += imageTokens
		default:
			chars += len(b.Text) + len(b.Thinking) + len(b.Name) + len(b.Arguments)
		}
	}
	return tokens + chars/4 + 4
}

// contextTokens estimates the tokens the active conversation sends: the usage
// the provider reported for the last answer plus an estimate for everything
// after it.
// contextTokens 估算当前对话的 token：取最后一次回答的实际用量，再加其后消息的估算。
func contextTokens(messages []Message) int {
	last := -1
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role == "assistant" && m.Usage != nil && m.StopReason != "aborted" && m.StopReason != "error" && m.Usage.Input+m.Usage.Output > 0 {
			last = i
			break
		}
	}
	total := 0
	if last >= 0 {
		u := messages[last].Usage
		total = u.Input + u.Output + u.CacheRead + u.CacheWrite
	}
	for _, m := range messages[last+1:] {
		total += estimateTokens(m)
	}
	return total
}

// compactionThreshold is the context size above which compaction runs.
func compactionThreshold(window, reserve int) int {
	if window <= 0 {
		return 0
	}
	if window <= 2*reserve {
		return window * 8 / 10
	}
	return window - reserve
}

// IsContextOverflow reports whether a provider error says the request did not
// fit the model's context window.
func IsContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{"context_length_exceeded", "maximum context length", "context window", "too many tokens", "prompt is too long", "exceeds the context", "input is too long", "context length"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// validCut reports whether kept history may start at this message; a tool
// result must stay with the call that produced it.
func validCut(m Message) bool {
	switch m.Role {
	case "user", "assistant", "custom", "bashExecution", "branchSummary":
		return true
	}
	return false
}

// compact summarizes the history before the most recent kept tokens and
// appends a compaction entry. The caller holds no lock.
// compact 把最近保留部分之前的历史生成摘要，并追加压缩条目；调用方不持锁。
func (a *Agent) compact(ctx context.Context, reason, instructions string, willRetry bool) (*CompactionResult, error) {
	plan, err := a.planCompaction()
	if err != nil {
		return nil, err
	}
	a.emit(map[string]any{"type": "compaction_start", "reason": reason})
	result, err := a.runCompaction(ctx, plan, instructions)
	end := map[string]any{"type": "compaction_end", "reason": reason, "result": result, "aborted": ctx.Err() != nil, "willRetry": willRetry && err == nil}
	if err != nil {
		end["result"] = nil
		if ctx.Err() == nil {
			end["errorMessage"] = err.Error()
		}
	}
	a.emit(end)
	return result, err
}

// compactionPlan is the part of the active conversation a compaction
// summarizes: items[start:cut], with items[cut] the first entry kept.
type compactionPlan struct {
	model        Model
	items        []contextItem
	previous     *Message
	start, cut   int
	tokensBefore int
}

// planCompaction chooses the cut point without side effects.
func (a *Agent) planCompaction() (*compactionPlan, error) {
	a.mu.Lock()
	items, _ := a.session.contextItems()
	model := a.model
	a.mu.Unlock()
	if model == nil {
		return nil, ErrNoModel
	}
	messages := make([]Message, len(items))
	for i, item := range items {
		messages[i] = item.message
	}
	plan := &compactionPlan{model: *model, items: items, tokensBefore: contextTokens(messages)}
	if len(items) > 0 && items[0].message.Role == "compactionSummary" {
		plan.previous = &items[0].message
		plan.start = 1
	}
	// Walk back from the newest message until the recent budget are kept, then
	// move to the nearest message where kept history may start.
	cut := len(items)
	kept := 0
	for cut > plan.start && kept < a.opts.KeepRecentTokens {
		cut--
		kept += estimateTokens(items[cut].message)
	}
	for cut > plan.start && (!validCut(items[cut].message) || items[cut].entryID == "") {
		cut--
	}
	if cut <= plan.start {
		return nil, ErrNothingToCompact
	}
	plan.cut = cut
	return plan, nil
}

func (a *Agent) runCompaction(ctx context.Context, plan *compactionPlan, instructions string) (*CompactionResult, error) {
	items, previous, start, cut, model, tokensBefore := plan.items, plan.previous, plan.start, plan.cut, &plan.model, plan.tokensBefore
	files := collectFiles(previous, items[start:cut])
	var prompt strings.Builder
	prompt.WriteString("<conversation>\n")
	prompt.WriteString(serializeConversation(items[start:cut]))
	prompt.WriteString("</conversation>\n\n")
	if previous != nil {
		prompt.WriteString("<previous-summary>\n" + previous.Summary + "\n</previous-summary>\n\nUpdate the previous summary with the new conversation above.\n\n")
	}
	prompt.WriteString(summaryFormat)
	if strings.TrimSpace(instructions) != "" {
		prompt.WriteString("\n\nAdditional focus: " + strings.TrimSpace(instructions))
	}

	llm, err := a.opts.NewLLM(*model, a.opts.Catalog.Key(model.Provider))
	if err != nil {
		return nil, err
	}
	resp, err := llm.Chat(ctx, contract.ChatRequest{
		Model:     model.ID,
		Messages:  []contract.Message{{Role: "system", Content: summarySystemPrompt}, {Role: "user", Content: prompt.String()}},
		MaxTokens: a.opts.ReserveTokens * 8 / 10,
	})
	if err != nil {
		return nil, err
	}
	summary := strings.TrimSpace(resp.Content)
	if summary == "" {
		return nil, errors.New("the model returned an empty summary")
	}
	if len(files.ReadFiles) > 0 {
		summary += "\n\n<read-files>\n" + strings.Join(files.ReadFiles, "\n") + "\n</read-files>"
	}
	if len(files.ModifiedFiles) > 0 {
		summary += "\n\n<modified-files>\n" + strings.Join(files.ModifiedFiles, "\n") + "\n</modified-files>"
	}
	details, _ := json.Marshal(files)
	usage := usageFor(*model, resp.Usage)
	result := &CompactionResult{Summary: summary, FirstKeptEntryID: items[cut].entryID, TokensBefore: tokensBefore, Usage: usage, Details: details}

	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := a.session.Append(&Entry{Type: "compaction", Summary: summary, FirstKeptEntryID: result.FirstKeptEntryID, TokensBefore: tokensBefore, Usage: usage, Details: details}); err != nil {
		return nil, err
	}
	after := 0
	for _, m := range a.session.Context().Messages {
		after += estimateTokens(m)
	}
	result.EstimatedTokensAfter = after
	return result, nil
}

// serializeConversation renders messages as labelled text so the summarizer
// does not mistake them for a conversation to continue.
func serializeConversation(items []contextItem) string {
	var b strings.Builder
	for _, item := range items {
		m := item.message
		switch m.Role {
		case "user", "custom":
			fmt.Fprintf(&b, "[User]: %s\n", contentText(m.Content, false))
		case "assistant":
			for _, block := range m.Content {
				if block.Type == "thinking" && block.Thinking != "" {
					fmt.Fprintf(&b, "[Assistant thinking]: %s\n", block.Thinking)
				}
			}
			if text := m.Text(); text != "" {
				fmt.Fprintf(&b, "[Assistant]: %s\n", text)
			}
			var calls []string
			for _, block := range m.Content {
				if block.Type == "toolCall" {
					calls = append(calls, fmt.Sprintf("%s(%s)", block.Name, string(block.Arguments)))
				}
			}
			if len(calls) > 0 {
				fmt.Fprintf(&b, "[Assistant tool calls]: %s\n", strings.Join(calls, "; "))
			}
		case "toolResult":
			text := contentText(m.Content, false)
			if len(text) > maxToolResultLen {
				text = text[:maxToolResultLen] + fmt.Sprintf("\n[... %d more characters truncated]", len(text)-maxToolResultLen)
			}
			fmt.Fprintf(&b, "[Tool result]: %s\n", text)
		case "bashExecution":
			fmt.Fprintf(&b, "[User ran]: %s\n%s\n", m.Command, m.Output)
		case "branchSummary", "compactionSummary":
			fmt.Fprintf(&b, "[Summary]: %s\n", m.Summary)
		}
	}
	return b.String()
}

// collectFiles gathers the files read and modified by tool calls, carrying
// forward the lists of the previous compaction.
func collectFiles(previous *Message, items []contextItem) fileDetails {
	read, modified := map[string]bool{}, map[string]bool{}
	if previous != nil && len(previous.Details) > 0 {
		var prior fileDetails
		if json.Unmarshal(previous.Details, &prior) == nil {
			for _, f := range prior.ReadFiles {
				read[f] = true
			}
			for _, f := range prior.ModifiedFiles {
				modified[f] = true
			}
		}
	}
	for _, item := range items {
		if item.message.Role != "assistant" {
			continue
		}
		for _, block := range item.message.Content {
			if block.Type != "toolCall" {
				continue
			}
			var args struct {
				Path string `json:"path"`
			}
			if json.Unmarshal(block.Arguments, &args) != nil || args.Path == "" {
				continue
			}
			switch block.Name {
			case "read":
				read[args.Path] = true
			case "write", "edit":
				modified[args.Path] = true
			}
		}
	}
	for f := range modified {
		delete(read, f)
	}
	return fileDetails{ReadFiles: sortedKeys(read), ModifiedFiles: sortedKeys(modified)}
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
