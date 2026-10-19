// Package requestcheck validates constraints shared by native protocol adapters.
// It does not select providers or change the contract's default behavior.
package requestcheck

import (
	"encoding/json"
	"fmt"

	"github.com/jinyitao123/loom/contract"
)

// Validate rejects constraints that cannot be represented faithfully on the wire.
func Validate(req contract.ChatRequest) error {
	if req.Effort != "" && req.Effort != contract.EffortLow && req.Effort != contract.EffortMedium && req.Effort != contract.EffortHigh {
		return fmt.Errorf("invalid reasoning effort %q", req.Effort)
	}
	if req.Schema != nil {
		var schema map[string]json.RawMessage
		if json.Unmarshal(*req.Schema, &schema) != nil || schema == nil {
			return fmt.Errorf("output schema must be a JSON object")
		}
	}
	for _, m := range req.Messages {
		for _, c := range m.ToolCalls {
			if c.Args != "" && !json.Valid([]byte(c.Args)) {
				return fmt.Errorf("invalid arguments in tool history")
			}
		}
	}
	c := req.ToolChoice
	if c == nil {
		return nil
	}
	if len(req.Tools) == 0 {
		return fmt.Errorf("tool choice requires offered tools")
	}
	switch c.Mode {
	case contract.ToolChoiceAuto, contract.ToolChoiceNone, contract.ToolChoiceRequired:
		if c.Name != "" {
			return fmt.Errorf("only a named tool choice may carry a name")
		}
	case contract.ToolChoiceTool:
		for _, t := range req.Tools {
			if c.Name != "" && c.Name == t.Name {
				return nil
			}
		}
		return fmt.Errorf("named tool is not offered")
	default:
		return fmt.Errorf("unknown tool choice mode %q", c.Mode)
	}
	return nil
}
