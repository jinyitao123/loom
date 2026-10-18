package interactive

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

// promptGraphName names the graph that runs one prompt; its checkpoints live
// in a per-run MemStore.
const promptGraphName = "interactive-prompt"

// maxOutputTokens caps the output limit sent with each model request.
const maxOutputTokens = 32768

// runPrompt runs queued prompts as one turn. The work itself is a Loom graph
// (see runGraph); this method records the user messages, emits the turn
// events and, when the model reports the context is too long, compacts the
// session and runs a fresh graph from it once.
// runPrompt 把排队的提问作为一轮执行：执行本身是一张 Loom 图（见 runGraph）；
// 这里记录用户消息、发出轮次事件，模型报上下文过长时压缩会话并从会话重新起一次图运行。
func (a *Agent) runPrompt(ctx context.Context, items []queued) {
	a.mu.Lock()
	model, thinking := *a.model, a.thinking
	a.mu.Unlock()

	run := &promptRun{agent: a, model: model, thinking: thinking, tools: map[string]toolCallInfo{}}
	a.setCurrentRun(run)
	defer a.setCurrentRun(nil)
	a.emit(map[string]any{"type": "agent_start"})
	a.emit(map[string]any{"type": "turn_start"})
	var prompts []string
	for _, item := range items {
		run.appendAndAnnounce(a.userMessage(item))
		prompts = append(prompts, item.text)
	}
	prompt := strings.Join(prompts, "\n\n")

	err := a.runGraph(ctx, run, prompt)
	if err != nil && ctx.Err() == nil && IsContextOverflow(err) && a.autoCompactionEnabled() {
		if _, cerr := a.compact(ctx, "overflow", "", true); cerr == nil {
			run.lastErr = nil
			err = a.runGraph(ctx, run, prompt)
		}
	}
	if err != nil && ctx.Err() == nil && !errors.Is(err, run.lastErr) {
		run.appendAndAnnounce(Message{Role: "assistant", API: model.API, Provider: model.Provider, Model: model.ID,
			Usage: &Usage{}, StopReason: "error", ErrorMessage: err.Error(), Timestamp: nowMillis()})
	}
	a.emit(map[string]any{"type": "turn_end", "message": run.assistant, "toolResults": nonNil(run.toolResults)})
	a.emit(map[string]any{"type": "agent_end", "messages": nonNil(run.added), "willRetry": false})
}

// runGraph runs the turn as a graph of two steps:
//
//	prepare → round
//
// prepare tells the capability servers a turn begins and loads the active
// conversation from the session file; round is a controlled stdlib tool loop
// that pauses after every model round. At each pause the agent delivers queued
// steering through the resume grant and resumes the graph; any other pause
// (output limit, provider stop, repeated calls, round budget) ends the turn.
// runGraph 以两步图执行本轮：prepare 通知能力服务器并从会话文件载入上下文；
// round 是每轮模型调用后暂停的受控工具循环。暂停时经续跑授权送达排队的插话后续跑；
// 其他暂停原因（输出上限、提供方停止、重复调用、轮次预算）结束本轮。
func (a *Agent) runGraph(ctx context.Context, run *promptRun, prompt string) error {
	llm, err := a.opts.NewLLM(run.model, a.opts.Catalog.Key(run.model.Provider))
	if err != nil {
		return err
	}
	vision := supportsImages(run.model)
	maxTokens := run.model.MaxTokens
	if maxTokens <= 0 || maxTokens > maxOutputTokens {
		maxTokens = maxOutputTokens
	}
	round := stdlib.NewToolLoopStep(&observedLLM{inner: llm, run: run}, a.opts.Tools, stdlib.ToolLoopOpts{
		Model:         run.model.ID,
		SystemPrompt:  a.opts.SystemPrompt,
		MaxIterations: 1, // one model round per slice: the loop pauses between rounds
		MaxTokens:     maxTokens,
		ToolHooks:     []contract.ToolHook{run.toolHook()},
		Compaction:    a.compactionPolicy(run, vision),
		Control:       &stdlib.ToolLoopControl{ID: "prompt", InitialTotalRounds: uint64(a.opts.MaxIterations)},
	})
	graph := loom.NewGraph(promptGraphName, "prepare", loom.WithMergeConfig(loom.NewMergeConfig()))
	graph.AddStep("prepare", a.prepareStep(prompt, vision), loom.Always("round"))
	graph.AddStep("round", round, loom.End())
	store := loom.NewMemStore()

	result, err := graph.Run(ctx, loom.State{}, store)
	for grants := 1; err == nil && result.Yielded; grants++ {
		outcome, present, oerr := stdlib.ReadToolLoopOutcome(result.State)
		if oerr != nil {
			return oerr
		}
		if !present || outcome.Reason != stdlib.ToolLoopSliceLimit {
			return nil
		}
		seq, ok := stateInt64(result.State["__checkpoint_seq"])
		token, _ := result.State["__yield_token"].(string)
		if !ok || token == "" {
			return fmt.Errorf("interactive: paused turn has no resume boundary")
		}
		steering, input := a.takeSteering(vision)
		delta, perr := stdlib.PrepareToolLoopResume(result.State, stdlib.ToolLoopResumeGrant{
			ID: fmt.Sprintf("round-%d", grants), ExpectedRunID: result.RunID, ExpectedCheckpointSeq: seq,
			ExpectedYieldToken: token, ExpectedSlice: outcome.Slice,
			AuthorizedTotalRounds: outcome.AuthorizedTotalRounds, AppendMessages: input,
		})
		if perr != nil {
			return perr
		}
		// The steering joins the session only once the loop accepted it.
		for _, m := range steering {
			run.appendAndAnnounce(m)
		}
		result, err = graph.Resume(ctx, result.RunID, delta, store)
	}
	return err
}

// prepareStep starts a turn: it lets the capability servers bind the turn's
// prompt, then loads the active conversation (compactions applied) as the
// loop's messages.
func (a *Agent) prepareStep(prompt string, vision bool) loom.Step {
	return func(ctx context.Context, _ loom.State) (loom.State, error) {
		if a.opts.OnTurnStart != nil {
			if err := a.opts.OnTurnStart(ctx, prompt); err != nil {
				a.emit(map[string]any{"type": "extension_error", "extensionPath": "turn", "event": "turn_start", "error": err.Error()})
			}
		}
		a.mu.Lock()
		messages := toContract(a.session.Context().Messages, vision)
		a.mu.Unlock()
		return loom.State{"messages": messages}, nil
	}
}

// takeSteering removes the steering due at this round boundary and returns
// the session messages with their model form.
func (a *Agent) takeSteering(vision bool) ([]Message, []contract.Message) {
	a.mu.Lock()
	items := takeLocked(&a.steering, a.steeringMode)
	if items != nil {
		a.emitQueueLocked()
	}
	a.mu.Unlock()
	var stored []Message
	for _, item := range items {
		stored = append(stored, a.userMessage(item))
	}
	return stored, toContract(stored, vision)
}

// compactionPolicy plugs session compaction into the tool loop's compaction
// hook: it triggers when the session context passes the threshold, appends a
// compaction entry, and hands the loop the transcript rebuilt from the
// session. A failed or empty compaction leaves the transcript as it is and is
// not retried within the turn.
// compactionPolicy 把会话压缩接入工具循环的压缩钩子：会话上下文超过阈值时触发，
// 追加压缩条目并把从会话重建的对话交给循环；压缩失败或无可压缩时保持原样，本轮不再重试。
func (a *Agent) compactionPolicy(run *promptRun, vision bool) *stdlib.CompactionPolicy {
	return &stdlib.CompactionPolicy{
		Trigger: func([]contract.Message, int) bool { return !run.compactionFailed && a.needsCompaction() },
		Compactor: func(ctx context.Context, messages []contract.Message) ([]contract.Message, error) {
			if _, err := a.compact(ctx, "threshold", "", false); err != nil {
				run.compactionFailed = true
				return messages, nil
			}
			return a.loopMessages(messages, vision), nil
		},
	}
}

func stateInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	case float64:
		return int64(v), v == float64(int64(v))
	}
	return 0, false
}
