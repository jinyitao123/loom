package stdlib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jinyitao123/loom/contract"
)

// ToolChoiceInput is the read-only context a host policy receives before one
// model round of a ToolLoop. It carries no transcript: hosts decide from their
// own authoritative facts, so the policy stays independent of model text.
type ToolChoiceInput struct {
	// Tools are the tools offered in this round.
	Tools []contract.ToolDef
	// CompletionRejected reports that the previous model round proposed a final
	// response which the CompletionVerifier rejected.
	CompletionRejected bool
	// CompletionRejectionReason is that decision's machine-readable Reason; it
	// may be empty when the verifier did not supply one.
	CompletionRejectionReason string
}

// ToolChoicePolicy selects an optional tool calling constraint for one model
// round. Returning nil keeps the provider default for that round. Policies must
// be deterministic for the same input and host facts so journal replay matches.
type ToolChoicePolicy interface {
	ChooseTool(context.Context, ToolChoiceInput) (*contract.ToolChoice, error)
}

// ToolChoicePolicyFunc adapts a function to ToolChoicePolicy.
type ToolChoicePolicyFunc func(context.Context, ToolChoiceInput) (*contract.ToolChoice, error)

func (choose ToolChoicePolicyFunc) ChooseTool(ctx context.Context, input ToolChoiceInput) (*contract.ToolChoice, error) {
	return choose(ctx, input)
}

// ErrToolChoiceNotHonored means a model response contradicted the tool choice
// requested for that round, for example a required call that returned text only.
// The loop stops without dispatching any of that response's tool calls.
var ErrToolChoiceNotHonored = errors.New("model response did not honor the requested tool choice")

// toolLoopCompletionRejection is the checkpoint-safe record of the verifier
// rejection that the next model round responds to.
type toolLoopCompletionRejection struct {
	Reason string `json:"reason"`
}

const (
	maxCompletionRejectionReasonBytes = 256
	// maxCompletionRejections bounds the rejection history kept for verifiers;
	// beyond it the oldest reasons are dropped.
	maxCompletionRejections = 64
)

func boundedRejectionReason(reason string) string {
	if len(reason) > maxCompletionRejectionReasonBytes {
		reason = strings.ToValidUTF8(reason[:maxCompletionRejectionReasonBytes], "")
	}
	return reason
}

func newCompletionRejection(reason string) *toolLoopCompletionRejection {
	return &toolLoopCompletionRejection{Reason: boundedRejectionReason(reason)}
}

// appendCompletionRejection records one rejected candidate's Reason (possibly
// empty) for later verifiers, keeping the most recent bounded history.
func appendCompletionRejection(history []string, reason string) []string {
	history = append(append([]string(nil), history...), boundedRejectionReason(reason))
	if len(history) > maxCompletionRejections {
		history = history[len(history)-maxCompletionRejections:]
	}
	return history
}

// decodeCompletionRejections restores the legacy loop's private history after
// checkpoint JSON round-tripping. Absent means no rejection was recorded.
func decodeCompletionRejections(raw any) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	if history, ok := raw.([]string); ok {
		return history, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("loom/toolloop: encode __toolloop_rejections: %w", err)
	}
	var history []string
	if err := json.Unmarshal(data, &history); err != nil || len(history) > maxCompletionRejections {
		return nil, fmt.Errorf("loom/toolloop: invalid __toolloop_rejections")
	}
	for _, reason := range history {
		if len(reason) > maxCompletionRejectionReasonBytes {
			return nil, fmt.Errorf("loom/toolloop: invalid __toolloop_rejections")
		}
	}
	return history, nil
}

// chooseRoundTool asks the optional policy for this round and validates the
// answer against the offered tools before any provider request is built.
func chooseRoundTool(ctx context.Context, opts ToolLoopOpts, tools []contract.ToolDef, rejection *toolLoopCompletionRejection) (*contract.ToolChoice, error) {
	if opts.ToolChoicePolicy == nil {
		return nil, nil
	}
	input := ToolChoiceInput{Tools: append([]contract.ToolDef(nil), tools...)}
	if rejection != nil {
		input.CompletionRejected, input.CompletionRejectionReason = true, rejection.Reason
	}
	choice, err := opts.ToolChoicePolicy.ChooseTool(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("loom/toolloop: choose tool: %w", err)
	}
	if choice == nil {
		return nil, nil
	}
	copy := *choice
	if len(tools) == 0 {
		return nil, fmt.Errorf("loom/toolloop: a tool choice requires offered tools")
	}
	switch copy.Mode {
	case contract.ToolChoiceAuto, contract.ToolChoiceNone, contract.ToolChoiceRequired:
		if copy.Name != "" {
			return nil, fmt.Errorf("loom/toolloop: only a named tool choice carries a tool name")
		}
	case contract.ToolChoiceTool:
		offered := false
		for _, tool := range tools {
			offered = offered || tool.Name == copy.Name
		}
		if strings.TrimSpace(copy.Name) == "" || !offered {
			return nil, fmt.Errorf("loom/toolloop: the chosen tool is not offered")
		}
	default:
		return nil, fmt.Errorf("loom/toolloop: unknown tool choice mode")
	}
	return &copy, nil
}

// toolChoiceHonored checks a response against the round's requested choice.
func toolChoiceHonored(choice *contract.ToolChoice, calls []contract.ToolCall) bool {
	if choice == nil {
		return true
	}
	switch choice.Mode {
	case contract.ToolChoiceNone:
		return len(calls) == 0
	case contract.ToolChoiceRequired:
		return len(calls) > 0
	case contract.ToolChoiceTool:
		if len(calls) == 0 {
			return false
		}
		for _, call := range calls {
			if call.Name != choice.Name {
				return false
			}
		}
	}
	return true
}

func toolChoiceNotHonored(choice *contract.ToolChoice) error {
	return fmt.Errorf("loom/toolloop: %w (%s)", ErrToolChoiceNotHonored, choice.Mode)
}
