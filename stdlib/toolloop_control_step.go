package stdlib

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
)

// This opt-in path shares dispatch, patch validation and park value types with
// the legacy loop. Local state is private; only Graph merges the returned delta.
func newControlledToolLoopStep(llm contract.LLM, tools contract.ToolDispatcher, opts ToolLoopOpts) loom.Step {
	controlOpts := *opts.Control
	opts.Control = &controlOpts
	if opts.MaxToolRepeats <= 0 {
		opts.MaxToolRepeats = 3
	}
	if opts.Effort == "" {
		opts.Effort = contract.EffortMedium
	}
	return func(ctx context.Context, state loom.State) (loom.State, error) {
		// This guards only this step's model/tool calls. Earlier child steps may
		// already have run; whole-topology admission belongs to the host.
		if nested, _ := ctx.Value(childContinuationContextKey{}).(bool); nested {
			return nil, fmt.Errorf("loom/toolloop: controlled continuation is not supported in child graphs")
		}
		if strings.TrimSpace(controlOpts.ID) == "" || controlOpts.InitialTotalRounds == 0 ||
			controlOpts.InitialTotalRounds > maxToolLoopCounter || uint64(opts.MaxIterations) > maxToolLoopCounter {
			return nil, fmt.Errorf("loom/toolloop: invalid control configuration")
		}
		if (opts.CompletionVerifier == nil) != (strings.TrimSpace(opts.CompletionVerifierID) == "") {
			return nil, fmt.Errorf("loom/toolloop: controlled completion verifier requires a stable policy id")
		}
		runID, _ := state["__run_id"].(string)
		if runID == "" {
			return nil, fmt.Errorf("loom/toolloop: control requires a graph run identity")
		}
		policy, err := toolLoopPolicyHash(opts, state)
		if err != nil {
			return nil, err
		}
		control, present, err := readToolLoopControl(state)
		if err != nil {
			return nil, err
		}
		loop := controlledToolLoop{
			control: control, pending: []toolLoopPendingCall{}, stagedResults: []contract.ToolResult{},
			patch: loom.State{}, stagedJSON: map[string][]byte{}, stagedState: loom.State{},
		}
		for key, value := range state {
			loop.stagedState[key] = value
		}
		if present {
			if control.PolicySHA256 != policy || control.LoopID != controlOpts.ID || control.SliceLimit != uint64(opts.MaxIterations) {
				return nil, fmt.Errorf("loom/toolloop: control policy changed across continuation")
			}
			if err := controlJSON(state["__toolloop_msgs"], &loop.messages); err != nil {
				return nil, err
			}
			if err := controlJSON(state["__toolloop_pending"], &loop.pending); err != nil {
				return nil, err
			}
			if err := controlJSON(state["__toolloop_staged_patch"], &loop.stagedResults); err != nil {
				return nil, err
			}
			if err := controlJSON(state["__toolloop_usage"], &loop.usage); err != nil {
				return nil, err
			}
			if control.Phase == "complete" {
				return nil, fmt.Errorf("loom/toolloop: completed control cannot be executed again")
			}
			if control.Phase == "paused" && control.Reason != ToolLoopAwaitToolResult {
				return loop.pause(control.Reason, control.HaltedResponse), nil
			}
			if err := stageStatePatches(loop.stagedResults, opts.StatePatchPolicy, loop.stagedState, loop.patch, loop.stagedJSON); err != nil {
				return nil, err
			}
			if len(loop.pending) > 0 {
				results, err := decodeResumedToolResults(state["__resumed_tool_results"])
				if err != nil {
					return nil, err
				}
				known := make(map[string]bool, len(loop.pending))
				remaining := make([]toolLoopPendingCall, 0, len(loop.pending))
				for _, call := range loop.pending {
					known[call.CallID] = true
					result, ok := results[call.CallID]
					if !ok {
						remaining = append(remaining, call)
						continue
					}
					toolResult := contract.ToolResult{CallID: call.CallID, Content: result.Content, IsError: result.IsError, ToolName: call.Tool}
					loop.messages = append(loop.messages, toolResult.AsMessage())
				}
				for callID := range results {
					if !known[callID] {
						return nil, fmt.Errorf("loom/toolloop: resumed result %q is not pending", callID)
					}
				}
				loop.pending = remaining
				if len(remaining) > 0 {
					return loop.pause(ToolLoopAwaitToolResult, nil), nil
				}
				loop.control.Phase, loop.control.Reason = "active", ""
				if hasStopLoopResult(loop.stagedResults) {
					return loop.finish(ToolLoopToolStop, lastAssistantContent(loop.messages)), nil
				}
			}
		} else {
			for _, key := range controlSnapshotKeys[1:] {
				if _, exists := state[key]; exists {
					return nil, fmt.Errorf("loom/toolloop: control is missing from an existing private snapshot")
				}
			}
			messages, err := GetMessages(state)
			if err != nil {
				return nil, err
			}
			if err := controlJSON(messages, &loop.messages); err != nil {
				return nil, err
			}
			if loop.messages == nil {
				loop.messages = []contract.Message{}
			}
			system := opts.SystemPrompt
			if value, ok := state["__system_prompt"].(string); ok && value != "" {
				system = value
			}
			if system != "" && (len(loop.messages) == 0 || loop.messages[0].Role != "system") {
				loop.messages = append([]contract.Message{{Role: "system", Content: system}}, loop.messages...)
			}
			loop.control = toolLoopControlState{
				Version: 1, RunID: runID, LoopID: controlOpts.ID, PolicySHA256: policy, Phase: "active",
				Slice: 1, SliceLimit: uint64(opts.MaxIterations), AuthorizedTotalRounds: controlOpts.InitialTotalRounds,
			}
		}
		if err := validateControlTranscript(loop.messages, loop.pending); err != nil {
			return nil, err
		}
		if reason := loop.roundLimit(); reason != "" {
			return loop.pause(reason, nil), nil
		}
		availableTools, err := tools.ListTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("loom/toolloop: list tools: %w", err)
		}
		for {
			if reason := loop.roundLimit(); reason != "" {
				return loop.pause(reason, nil), nil
			}
			if opts.Compaction != nil {
				if opts.Compaction.Trigger == nil || opts.Compaction.Compactor == nil {
					return nil, fmt.Errorf("loom/toolloop: incomplete compaction policy")
				}
				if opts.Compaction.Trigger(loop.messages, estimateMessagesTokens(loop.messages)) {
					if opts.Compaction.PreCompact != nil {
						_ = opts.Compaction.PreCompact(ctx, loop.messages)
					}
					loop.messages, err = opts.Compaction.Compactor(ctx, loop.messages)
					if err != nil {
						return nil, fmt.Errorf("loom/toolloop: compaction failed: %w", err)
					}
					if err := validateControlTranscript(loop.messages, nil); err != nil {
						return nil, err
					}
				}
			}
			resp, err := llm.Chat(ctx, contract.ChatRequest{
				Model: opts.Model, Messages: loop.messages, Tools: availableTools,
				MaxTokens: opts.MaxTokens, Schema: opts.OutputSchema, Effort: opts.Effort,
			})
			if err != nil {
				return nil, err
			}
			if resp == nil {
				return nil, fmt.Errorf("loom/toolloop: model returned no response")
			}
			loop.control.SliceRoundsUsed++
			loop.control.TotalRoundsUsed++
			nextUsage := addUsage(loop.usage, resp.Usage)
			if resp.Usage.InputTokens < 0 || resp.Usage.OutputTokens < 0 || resp.Usage.CostUSD < 0 ||
				nextUsage.InputTokens < loop.usage.InputTokens || nextUsage.OutputTokens < loop.usage.OutputTokens ||
				math.IsNaN(nextUsage.CostUSD) || math.IsInf(nextUsage.CostUSD, 0) {
				return nil, fmt.Errorf("loom/toolloop: invalid model usage")
			}
			loop.usage = nextUsage
			switch resp.StopReason {
			case "length":
				return loop.pause(ToolLoopProviderLength, resp), nil
			case "stop":
				if len(resp.ToolCalls) != 0 {
					return nil, fmt.Errorf("loom/toolloop: stop response contains tool calls")
				}
			case "tool_calls":
				if len(resp.ToolCalls) == 0 {
					return nil, fmt.Errorf("loom/toolloop: tool_calls response contains no tool calls")
				}
			case "": // Existing adapters may omit a normalized provider stop reason.
			default:
				return loop.pause(ToolLoopProviderStop, resp), nil
			}
			if len(resp.ToolCalls) == 0 {
				accepted, continued, err := verifyToolLoopCompletion(ctx, opts, loop.messages, resp, loop.usage)
				if err != nil {
					return nil, err
				}
				if !accepted {
					loop.messages = continued
					continue
				}
				loop.messages = append(loop.messages, resp.AsMessage())
				return loop.finish(ToolLoopFinalResponse, resp.Content), nil
			}
			if err := validateControlTranscript([]contract.Message{resp.AsMessage()}, pendingCalls(resp.ToolCalls)); err != nil {
				return nil, err
			}
			batchHash := hashToolCalls(resp.ToolCalls)
			if batchHash == loop.control.LastBatchHash {
				loop.control.RepeatCount++
				if loop.control.RepeatCount >= uint64(opts.MaxToolRepeats) {
					return loop.pause(ToolLoopRepeatLimit, resp), nil
				}
			} else {
				loop.control.LastBatchHash, loop.control.RepeatCount = batchHash, 1
			}
			results, err := DispatchWithHooks(ctx, tools, resp.ToolCalls, availableTools, opts.ToolHooks)
			if err != nil {
				return nil, err
			}
			if err := stageStatePatches(results, opts.StatePatchPolicy, loop.stagedState, loop.patch, loop.stagedJSON); err != nil {
				return nil, err
			}
			loop.messages = append(loop.messages, resp.AsMessage())
			for i, result := range results {
				if result.CallID != resp.ToolCalls[i].ID || result.CallID == "" {
					return nil, fmt.Errorf("loom/toolloop: tool result identity does not match its call")
				}
				if result.StatePatch != nil || len(result.StateOps) > 0 {
					loop.stagedResults = append(loop.stagedResults, result)
				}
				if result.Park {
					call := resp.ToolCalls[i]
					loop.pending = append(loop.pending, toolLoopPendingCall{CallID: call.ID, Tool: call.Name, Args: call.Args, ParkRef: result.ParkRef})
				} else {
					loop.messages = append(loop.messages, result.AsMessage())
				}
			}
			if len(loop.pending) > 0 {
				return loop.pause(ToolLoopAwaitToolResult, nil), nil
			}
			if hasStopLoopResult(results) {
				return loop.finish(ToolLoopToolStop, resp.Content), nil
			}
		}
	}
}

func pendingCalls(calls []contract.ToolCall) []toolLoopPendingCall {
	pending := make([]toolLoopPendingCall, len(calls))
	for i, call := range calls {
		pending[i] = toolLoopPendingCall{CallID: call.ID, Tool: call.Name, Args: call.Args}
	}
	return pending
}

type controlledToolLoop struct {
	control       toolLoopControlState
	messages      []contract.Message
	pending       []toolLoopPendingCall
	stagedResults []contract.ToolResult
	usage         contract.Usage
	patch         loom.State
	stagedState   loom.State
	stagedJSON    map[string][]byte
}

func (loop *controlledToolLoop) roundLimit() ToolLoopStopReason {
	if loop.control.TotalRoundsUsed >= loop.control.AuthorizedTotalRounds {
		return ToolLoopTotalLimit
	}
	if loop.control.SliceRoundsUsed >= loop.control.SliceLimit {
		return ToolLoopSliceLimit
	}
	return ""
}

func (loop *controlledToolLoop) snapshot() loom.State {
	return loom.State{
		"__toolloop_control": loop.control, "__toolloop_msgs": loop.messages,
		"__toolloop_pending": loop.pending, "__toolloop_staged_patch": loop.stagedResults,
		"__toolloop_usage": loop.usage, "__resumed_tool_results": nil,
	}
}

func (loop *controlledToolLoop) pause(reason ToolLoopStopReason, response *contract.ChatResponse) loom.State {
	loop.control.Phase, loop.control.Reason = "paused", reason
	loop.control.HaltedResponse = response
	if response != nil {
		loop.control.ProviderStopReason, loop.control.PartialText = response.StopReason, response.Content
	}
	update := loop.snapshot()
	update["__yield"], update["__yield_phase"], update["yield_type"] = true, "mid_step", "toolloop_control"
	if reason == ToolLoopAwaitToolResult {
		update["yield_type"] = "await_approval"
	}
	return update
}

func (loop *controlledToolLoop) finish(reason ToolLoopStopReason, content string) loom.State {
	loop.control.Phase, loop.control.Reason = "complete", reason
	loop.control.PartialText, loop.control.ProviderStopReason, loop.control.HaltedResponse = "", "", nil
	update := loop.snapshot()
	for key, value := range finishToolLoop(content, loop.usage, false, loop.patch) {
		update[key] = value
	}
	update["__yield_phase"], update["yield_type"] = "after_step", nil
	return update
}
