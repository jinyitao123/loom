package stdlib

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
)

// CompactionPolicy controls automatic context compaction within a ToolLoop.
type CompactionPolicy struct {
	// Trigger decides when to compact. Receives messages and estimated token count.
	Trigger func(messages []contract.Message, tokenCount int) bool

	// Compactor summarizes old messages into a shorter form.
	Compactor func(ctx context.Context, messages []contract.Message) ([]contract.Message, error)

	// PreCompact (optional) is called before compaction for archival.
	PreCompact func(ctx context.Context, transcript []contract.Message) error
}

// ToolLoopOpts configures the tool loop step.
type ToolLoopOpts struct {
	Model         string
	SystemPrompt  string
	MaxIterations int
	OutputSchema  *json.RawMessage
	Effort        contract.EffortLevel  // v1.4: default EffortMedium
	ToolHooks     []contract.ToolHook   // v1.4: per-tool pre/post hooks
	Compaction    *CompactionPolicy     // v1.4: auto-summarize when context grows
}

// NewToolLoopStep creates the LLM → tool call → result → LLM loop step.
func NewToolLoopStep(llm contract.LLM, tools contract.ToolDispatcher, opts ToolLoopOpts) loom.Step {
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = 20
	}

	return func(ctx context.Context, state loom.State) (loom.State, error) {
		msgs, err := GetMessages(state)
		if err != nil {
			return nil, err
		}

		// Use __system_prompt from state if available, else fallback to opts.
		systemPrompt := opts.SystemPrompt
		if sp, ok := state["__system_prompt"].(string); ok && sp != "" {
			systemPrompt = sp
		}

		// Prepend system prompt if configured.
		if systemPrompt != "" && (len(msgs) == 0 || msgs[0].Role != "system") {
			msgs = append([]contract.Message{{Role: "system", Content: systemPrompt}}, msgs...)
		}

		// Cache tool list outside loop.
		availableTools, err := tools.ListTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("loom/toolloop: list tools: %w", err)
		}

		effort := opts.Effort
		if effort == "" {
			effort = contract.EffortMedium
		}

		for i := 0; i < opts.MaxIterations; i++ {
			// Context compaction check.
			if opts.Compaction != nil {
				tokenCount := estimateMessagesTokens(msgs)
				if opts.Compaction.Trigger(msgs, tokenCount) {
					if opts.Compaction.PreCompact != nil {
						opts.Compaction.PreCompact(ctx, msgs)
					}
					msgs, err = opts.Compaction.Compactor(ctx, msgs)
					if err != nil {
						return nil, fmt.Errorf("loom/toolloop: compaction failed: %w", err)
					}
				}
			}

			resp, err := llm.Chat(ctx, contract.ChatRequest{
				Model:    opts.Model,
				Messages: msgs,
				Tools:    availableTools,
				Schema:   opts.OutputSchema,
				Effort:   effort,
			})
			if err != nil {
				return loom.State{"__error": err.Error()}, err
			}

			if len(resp.ToolCalls) == 0 {
				return SetOutput(resp.Content, resp.Usage), nil
			}

			// Dispatch with hooks and read/write awareness.
			results := DispatchWithHooks(ctx, tools, resp.ToolCalls, availableTools, opts.ToolHooks)
			msgs = append(msgs, resp.AsMessage())
			for _, r := range results {
				msgs = append(msgs, r.AsMessage())
			}
		}
		return loom.State{"__error": "max tool iterations"}, fmt.Errorf("loom/toolloop: max iterations (%d) reached", opts.MaxIterations)
	}
}

// DispatchWithHooks runs tool hooks, then dispatches with read/write awareness.
func DispatchWithHooks(ctx context.Context, tools contract.ToolDispatcher,
	calls []contract.ToolCall, defs []contract.ToolDef, hooks []contract.ToolHook) []contract.ToolResult {

	// Build def lookup map.
	defMap := make(map[string]contract.ToolDef)
	for _, d := range defs {
		defMap[d.Name] = d
	}

	results := make([]contract.ToolResult, len(calls))

	for i, call := range calls {
		// Phase 1: run Pre hooks (can modify call or block).
		blocked := false
		for _, h := range hooks {
			if h.Matcher != nil && !h.Matcher(call.Name) {
				continue
			}
			if h.Pre != nil {
				modified, err := h.Pre(ctx, call)
				if err != nil {
					results[i] = contract.ToolResult{
						CallID:   call.ID,
						Content:  fmt.Sprintf("blocked by hook: %v", err),
						IsError:  true,
						ToolName: call.Name,
					}
					blocked = true
					break
				}
				call = modified
			}
		}

		if blocked {
			continue
		}

		calls[i] = call // store potentially modified call
	}

	// Phase 2: split by read-only vs stateful.
	type indexedCall struct {
		idx  int
		call contract.ToolCall
	}
	var readOnly, stateful []indexedCall
	for i, call := range calls {
		if results[i].CallID != "" {
			continue // already blocked
		}
		if d, ok := defMap[call.Name]; ok && d.ReadOnly {
			readOnly = append(readOnly, indexedCall{i, call})
		} else {
			stateful = append(stateful, indexedCall{i, call})
		}
	}

	// Read-only: parallel.
	if len(readOnly) > 0 {
		var wg sync.WaitGroup
		for _, ic := range readOnly {
			wg.Add(1)
			go func(idx int, c contract.ToolCall) {
				defer wg.Done()
				result, err := tools.Dispatch(ctx, c)
				if err != nil {
					results[idx] = contract.ToolResult{
						CallID: c.ID, Content: fmt.Sprintf("error: %v", err),
						IsError: true, ToolName: c.Name,
					}
					return
				}
				result.ToolName = c.Name
				results[idx] = *result
			}(ic.idx, ic.call)
		}
		wg.Wait()
	}

	// Stateful: serial (order matters).
	for _, ic := range stateful {
		result, err := tools.Dispatch(ctx, ic.call)
		if err != nil {
			results[ic.idx] = contract.ToolResult{
				CallID: ic.call.ID, Content: fmt.Sprintf("error: %v", err),
				IsError: true, ToolName: ic.call.Name,
			}
			continue
		}
		result.ToolName = ic.call.Name
		results[ic.idx] = *result
	}

	// Phase 3: run Post hooks.
	for i := range results {
		for _, h := range hooks {
			if h.Matcher != nil && !h.Matcher(results[i].ToolName) {
				continue
			}
			if h.Post != nil {
				h.Post(ctx, calls[i], &results[i])
			}
		}
	}

	return results
}

// estimateMessagesTokens provides a rough token count (~4 chars per token).
func estimateMessagesTokens(msgs []contract.Message) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content)/4 + 4 // content + overhead per message
	}
	return total
}
