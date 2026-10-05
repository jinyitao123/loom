package stdlib

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
)

// ToolLoopControl opts into durable model-round slices. Graph step budgets count
// graph steps, so they cannot account for model rounds inside one ToolLoop step.
type ToolLoopControl struct {
	ID                 string
	InitialTotalRounds uint64
}

// ToolLoopStopReason describes the loop outcome independently of Graph's yield.
type ToolLoopStopReason string

const (
	ToolLoopFinalResponse   ToolLoopStopReason = "final_response"
	ToolLoopToolStop        ToolLoopStopReason = "tool_stop"
	ToolLoopSliceLimit      ToolLoopStopReason = "slice_limit"
	ToolLoopTotalLimit      ToolLoopStopReason = "total_limit"
	ToolLoopRepeatLimit     ToolLoopStopReason = "repeat_limit"
	ToolLoopProviderLength  ToolLoopStopReason = "provider_length"
	ToolLoopProviderStop    ToolLoopStopReason = "provider_stop"
	ToolLoopAwaitToolResult ToolLoopStopReason = "await_tool_result"
)

// ToolLoopOutcome is a mechanism result, not a claim that a deliverable passed review.
type ToolLoopOutcome struct {
	Version               uint32
	RunID, LoopID         string
	Reason                ToolLoopStopReason
	Slice                 uint64
	SliceRoundsUsed       uint64
	TotalRoundsUsed       uint64
	AuthorizedTotalRounds uint64
	ProviderStopReason    string
	PartialText           string
}

// ToolLoopResumeGrant names one host-authorized continuation of an exact pause.
// AuthorizedTotalRounds is an absolute ceiling, never an amount to add repeatedly.
type ToolLoopResumeGrant struct {
	ID                    string
	ExpectedRunID         string
	ExpectedCheckpointSeq int64
	ExpectedYieldToken    string
	ExpectedSlice         uint64
	AuthorizedTotalRounds uint64
}

const maxToolLoopCounter uint64 = 1<<53 - 1

type toolLoopControlState struct {
	Version               uint32                 `json:"version"`
	RunID                 string                 `json:"run_id"`
	LoopID                string                 `json:"loop_id"`
	PolicySHA256          string                 `json:"policy_sha256"`
	Phase                 string                 `json:"phase"`
	Slice                 uint64                 `json:"slice"`
	SliceLimit            uint64                 `json:"slice_limit"`
	SliceStartRound       uint64                 `json:"slice_start_round"`
	SliceRoundsUsed       uint64                 `json:"slice_rounds_used"`
	TotalRoundsUsed       uint64                 `json:"total_rounds_used"`
	AuthorizedTotalRounds uint64                 `json:"authorized_total_rounds"`
	LastBatchHash         string                 `json:"last_batch_hash"`
	RepeatCount           uint64                 `json:"repeat_count"`
	LastGrantID           string                 `json:"last_grant_id"`
	Reason                ToolLoopStopReason     `json:"reason"`
	ProviderStopReason    string                 `json:"provider_stop_reason"`
	PartialText           string                 `json:"partial_text"`
	HaltedResponse        *contract.ChatResponse `json:"halted_response"`
	// LastCompletionRejection is present only between a rejected completion and
	// the next model round, so a pause at that boundary keeps the tool choice.
	// It is omitted otherwise, leaving snapshots of loops without it unchanged.
	LastCompletionRejection *toolLoopCompletionRejection `json:"last_completion_rejection,omitempty"`
	// CompletionRejections is the bounded rejection history given to verifiers
	// as CompletionCandidate.PriorRejections; omitted while empty.
	CompletionRejections []string `json:"completion_rejections,omitempty"`
}

var controlSnapshotKeys = []string{
	"__toolloop_control", "__toolloop_msgs", "__toolloop_pending",
	"__toolloop_staged_patch", "__toolloop_usage",
}

// ReadToolLoopOutcome returns present=false only when the control protocol is absent.
// Corrupt or mismatched protocol data is an error; active loops have an empty reason.
func ReadToolLoopOutcome(state loom.State) (ToolLoopOutcome, bool, error) {
	control, present, err := readToolLoopControl(state)
	if err != nil || !present {
		return ToolLoopOutcome{}, present, err
	}
	return ToolLoopOutcome{
		Version: control.Version, RunID: control.RunID, LoopID: control.LoopID,
		Reason: control.Reason, Slice: control.Slice, SliceRoundsUsed: control.SliceRoundsUsed,
		TotalRoundsUsed: control.TotalRoundsUsed, AuthorizedTotalRounds: control.AuthorizedTotalRounds,
		ProviderStopReason: control.ProviderStopReason, PartialText: control.PartialText,
	}, true, nil
}

// PrepareToolLoopResume returns a private state delta for Graph.Resume. It does
// not authorize, persist, or execute anything. Hosts must atomically compare the
// checkpoint and consume the grant before effects, and retain grant receipts.
func PrepareToolLoopResume(state loom.State, grant ToolLoopResumeGrant) (loom.State, error) {
	control, present, err := readToolLoopControl(state)
	if err != nil {
		return nil, err
	}
	if !present || control.Phase != "paused" ||
		(control.Reason != ToolLoopSliceLimit && control.Reason != ToolLoopTotalLimit) {
		return nil, fmt.Errorf("loom/toolloop: resume requires a controlled round-limit pause")
	}
	seq, seqOK := protocolInt64(state["__seq"])
	yieldSeq, yieldSeqOK := protocolInt64(state["__checkpoint_seq"])
	token, tokenOK := state["__yield_token"].(string)
	if !seqOK || !yieldSeqOK || seq <= 0 || uint64(seq) > maxToolLoopCounter || seq != yieldSeq || !tokenOK || token == "" ||
		state["__yield_phase"] != "mid_step" ||
		grant.ExpectedCheckpointSeq != seq || grant.ExpectedYieldToken != token ||
		grant.ExpectedRunID != control.RunID || grant.ExpectedSlice != control.Slice {
		return nil, fmt.Errorf("loom/toolloop: resume grant does not match the pause boundary")
	}
	if strings.TrimSpace(grant.ID) == "" || grant.ID == control.LastGrantID ||
		grant.AuthorizedTotalRounds < control.AuthorizedTotalRounds ||
		grant.AuthorizedTotalRounds <= control.TotalRoundsUsed ||
		grant.AuthorizedTotalRounds > maxToolLoopCounter || control.Slice == maxToolLoopCounter {
		return nil, fmt.Errorf("loom/toolloop: invalid or reused resume grant")
	}
	control.Phase, control.Reason = "active", ""
	control.Slice++
	control.SliceStartRound, control.SliceRoundsUsed = control.TotalRoundsUsed, 0
	control.AuthorizedTotalRounds, control.LastGrantID = grant.AuthorizedTotalRounds, grant.ID
	control.ProviderStopReason, control.PartialText, control.HaltedResponse = "", "", nil
	delta := make(loom.State, len(controlSnapshotKeys))
	for _, key := range controlSnapshotKeys {
		if err := cloneControlJSON(state[key], &delta, key); err != nil {
			return nil, err
		}
	}
	delta["__toolloop_control"] = control
	return delta, nil
}

func cloneControlJSON(value any, state *loom.State, key string) error {
	var copy any
	if err := controlJSON(value, &copy); err != nil {
		return err
	}
	(*state)[key] = copy
	return nil
}

func controlJSON(raw, target any) error {
	data, err := json.Marshal(raw)
	if err == nil {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		err = decoder.Decode(target)
	}
	if err != nil {
		return fmt.Errorf("loom/toolloop: invalid control snapshot: %w", err)
	}
	return nil
}

func readToolLoopControl(state loom.State) (toolLoopControlState, bool, error) {
	var control toolLoopControlState
	raw, present := state["__toolloop_control"]
	if !present {
		return control, false, nil
	}
	invalid := func() (toolLoopControlState, bool, error) {
		return control, true, fmt.Errorf("loom/toolloop: invalid control snapshot")
	}
	var fields map[string]json.RawMessage
	if err := controlJSON(raw, &fields); err != nil || fields == nil {
		return invalid()
	}
	// Required zero-valued fields are explicit, never silently supplied by decoding.
	for _, key := range []string{"version", "run_id", "loop_id", "policy_sha256", "phase", "slice", "slice_limit",
		"slice_start_round", "slice_rounds_used", "total_rounds_used", "authorized_total_rounds",
		"last_batch_hash", "repeat_count", "last_grant_id", "reason", "provider_stop_reason", "partial_text", "halted_response"} {
		value, ok := fields[key]
		if !ok || (string(value) == "null" && key != "halted_response") {
			return invalid()
		}
	}
	if err := controlJSON(raw, &control); err != nil {
		return control, true, err
	}
	for _, key := range controlSnapshotKeys {
		if value, ok := state[key]; !ok || value == nil {
			return invalid()
		}
	}
	if control.Version != 1 || control.RunID == "" || state["__run_id"] != control.RunID ||
		strings.TrimSpace(control.LoopID) == "" || !validControlHash(control.PolicySHA256) ||
		control.Slice == 0 || control.Slice > maxToolLoopCounter || control.SliceLimit == 0 || control.SliceLimit > maxToolLoopCounter ||
		control.SliceStartRound > maxToolLoopCounter || control.SliceRoundsUsed > control.SliceLimit ||
		control.TotalRoundsUsed != control.SliceStartRound+control.SliceRoundsUsed ||
		control.TotalRoundsUsed > control.AuthorizedTotalRounds || control.AuthorizedTotalRounds == 0 ||
		control.AuthorizedTotalRounds > maxToolLoopCounter || control.RepeatCount > control.TotalRoundsUsed ||
		(control.LastBatchHash == "") != (control.RepeatCount == 0) ||
		(control.LastBatchHash != "" && !validControlHash(control.LastBatchHash)) ||
		(control.Slice == 1 && (control.SliceStartRound != 0 || control.LastGrantID != "")) ||
		(control.Slice > 1 && strings.TrimSpace(control.LastGrantID) == "") {
		return invalid()
	}
	if rejection := control.LastCompletionRejection; rejection != nil &&
		(control.Phase == "complete" || len(rejection.Reason) > maxCompletionRejectionReasonBytes) {
		return invalid()
	}
	if len(control.CompletionRejections) > maxCompletionRejections {
		return invalid()
	}
	for _, reason := range control.CompletionRejections {
		if len(reason) > maxCompletionRejectionReasonBytes {
			return invalid()
		}
	}
	var pending []toolLoopPendingCall
	var msgs []contract.Message
	var staged []contract.ToolResult
	var usage contract.Usage
	if controlJSON(state["__toolloop_pending"], &pending) != nil ||
		controlJSON(state["__toolloop_msgs"], &msgs) != nil ||
		controlJSON(state["__toolloop_staged_patch"], &staged) != nil ||
		controlJSON(state["__toolloop_usage"], &usage) != nil ||
		pending == nil || msgs == nil || staged == nil || usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CostUSD < 0 {
		return invalid()
	}
	switch control.Phase {
	case "active":
		if control.Reason != "" || len(pending) != 0 || control.HaltedResponse != nil || control.PartialText != "" || control.ProviderStopReason != "" {
			return invalid()
		}
	case "complete":
		if (control.Reason != ToolLoopFinalResponse && control.Reason != ToolLoopToolStop) || len(pending) != 0 || control.HaltedResponse != nil {
			return invalid()
		}
	case "paused":
		switch control.Reason {
		case ToolLoopSliceLimit:
			if control.SliceRoundsUsed != control.SliceLimit || control.TotalRoundsUsed >= control.AuthorizedTotalRounds {
				return invalid()
			}
		case ToolLoopTotalLimit:
			if control.TotalRoundsUsed != control.AuthorizedTotalRounds {
				return invalid()
			}
		case ToolLoopRepeatLimit, ToolLoopProviderLength, ToolLoopProviderStop:
			if control.HaltedResponse == nil {
				return invalid()
			}
		case ToolLoopAwaitToolResult:
			if len(pending) == 0 {
				return invalid()
			}
		default:
			return invalid()
		}
		if control.Reason != ToolLoopAwaitToolResult && len(pending) != 0 {
			return invalid()
		}
		if control.Reason == ToolLoopSliceLimit || control.Reason == ToolLoopTotalLimit || control.Reason == ToolLoopAwaitToolResult {
			if control.HaltedResponse != nil || control.ProviderStopReason != "" || control.PartialText != "" {
				return invalid()
			}
		}
		if control.HaltedResponse != nil &&
			(control.HaltedResponse.StopReason != control.ProviderStopReason || control.HaltedResponse.Content != control.PartialText) {
			return invalid()
		}
		if control.Reason == ToolLoopProviderLength && control.ProviderStopReason != "length" {
			return invalid()
		}
		if control.Reason == ToolLoopProviderStop && (control.ProviderStopReason == "" || control.ProviderStopReason == "stop" ||
			control.ProviderStopReason == "tool_calls" || control.ProviderStopReason == "length") {
			return invalid()
		}
		if control.Reason == ToolLoopRepeatLimit && (len(control.HaltedResponse.ToolCalls) == 0 ||
			control.RepeatCount < 2 || hashToolCalls(control.HaltedResponse.ToolCalls) != control.LastBatchHash) {
			return invalid()
		}
	default:
		return invalid()
	}
	if err := validateToolTranscript(msgs, pending); err != nil {
		return control, true, err
	}
	return control, true, nil
}

// validateToolTranscript checks the tool-call/result pairing used by both
// ordinary and controlled loops before either sends history to a provider.
func validateToolTranscript(messages []contract.Message, pending []toolLoopPendingCall) error {
	unresolved := map[string]contract.ToolCall{}
	for _, message := range messages {
		if message.Role == "tool" {
			if _, ok := unresolved[message.ToolCallID]; !ok {
				return fmt.Errorf("loom/toolloop: transcript has an unmatched tool result")
			}
			delete(unresolved, message.ToolCallID)
			continue
		}
		if len(unresolved) > 0 {
			return fmt.Errorf("loom/toolloop: transcript has unresolved tool calls")
		}
		if len(message.ToolCalls) > 0 && message.Role != "assistant" {
			return fmt.Errorf("loom/toolloop: tool calls must belong to an assistant message")
		}
		for _, call := range message.ToolCalls {
			if _, duplicate := unresolved[call.ID]; duplicate || call.ID == "" || call.Name == "" {
				return fmt.Errorf("loom/toolloop: invalid tool call identity in transcript")
			}
			unresolved[call.ID] = call
		}
	}
	for _, call := range pending {
		original, ok := unresolved[call.CallID]
		if !ok || original.Name != call.Tool || original.Args != call.Args {
			return fmt.Errorf("loom/toolloop: pending call does not match the transcript")
		}
		delete(unresolved, call.CallID)
	}
	if len(unresolved) > 0 {
		return fmt.Errorf("loom/toolloop: transcript is missing pending tool calls")
	}
	return nil
}

func validControlHash(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == sha256.Size
}

func toolLoopPolicyHash(opts ToolLoopOpts, state loom.State) (string, error) {
	system := opts.SystemPrompt
	if value, ok := state["__system_prompt"].(string); ok && value != "" {
		system = value
	}
	if opts.ToolChoicePolicy != nil || opts.ToolChoicePolicyID != "" {
		// Only loops that opt into a tool choice policy use this identity shape;
		// existing continuation hashes below stay byte-for-byte unchanged.
		data, err := json.Marshal(struct {
			Version                     uint32
			Control                     ToolLoopControl
			Model, System               string
			Iterations, Repeats, Tokens int
			Effort                      contract.EffortLevel
			Schema                      *json.RawMessage
			CompletionVerifierID        string
			ToolChoicePolicyID          string
		}{3, *opts.Control, opts.Model, system, opts.MaxIterations, opts.MaxToolRepeats, opts.MaxTokens, opts.Effort, opts.OutputSchema, opts.CompletionVerifierID, opts.ToolChoicePolicyID})
		if err != nil {
			return "", fmt.Errorf("loom/toolloop: invalid control policy: %w", err)
		}
		digest := sha256.Sum256(data)
		return hex.EncodeToString(digest[:]), nil
	}
	if opts.CompletionVerifier == nil && opts.CompletionVerifierID == "" {
		data, err := json.Marshal(struct {
			Version                     uint32
			Control                     ToolLoopControl
			Model, System               string
			Iterations, Repeats, Tokens int
			Effort                      contract.EffortLevel
			Schema                      *json.RawMessage
		}{1, *opts.Control, opts.Model, system, opts.MaxIterations, opts.MaxToolRepeats, opts.MaxTokens, opts.Effort, opts.OutputSchema})
		if err != nil {
			return "", fmt.Errorf("loom/toolloop: invalid control policy: %w", err)
		}
		digest := sha256.Sum256(data)
		return hex.EncodeToString(digest[:]), nil
	}
	data, err := json.Marshal(struct {
		Version                     uint32
		Control                     ToolLoopControl
		Model, System               string
		Iterations, Repeats, Tokens int
		Effort                      contract.EffortLevel
		Schema                      *json.RawMessage
		CompletionVerifierID        string
	}{2, *opts.Control, opts.Model, system, opts.MaxIterations, opts.MaxToolRepeats, opts.MaxTokens, opts.Effort, opts.OutputSchema, opts.CompletionVerifierID})
	if err != nil {
		return "", fmt.Errorf("loom/toolloop: invalid control policy: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
