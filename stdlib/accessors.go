package stdlib

import (
	"fmt"

	"github.com/anthropic/loom"
	"github.com/anthropic/loom/contract"
)

// GetMessages retrieves conversation history with type checking.
func GetMessages(s loom.State) ([]contract.Message, error) {
	raw, ok := s["messages"]
	if !ok {
		return nil, nil
	}
	// Handle both typed and untyped (post-JSON roundtrip) messages.
	switch msgs := raw.(type) {
	case []contract.Message:
		return msgs, nil
	case []any:
		// After JSON roundtrip, messages are []any of map[string]any.
		var result []contract.Message
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				msg := contract.Message{
					Role:    stringVal(mm, "role"),
					Content: stringVal(mm, "content"),
				}
				if tcid, ok := mm["tool_call_id"].(string); ok {
					msg.ToolCallID = tcid
				}
				result = append(result, msg)
			} else if typed, ok := m.(contract.Message); ok {
				result = append(result, typed)
			}
		}
		return result, nil
	default:
		return nil, fmt.Errorf("loom: messages is %T, want []Message", raw)
	}
}

func stringVal(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// MustGetMessages panics on type mismatch.
func MustGetMessages(s loom.State) []contract.Message {
	msgs, err := GetMessages(s)
	if err != nil {
		panic(err)
	}
	return msgs
}

// GetString retrieves a string value with a default fallback.
func GetString(s loom.State, key, fallback string) string {
	if v, ok := s[key].(string); ok {
		return v
	}
	return fallback
}

// GetFloat retrieves a float64 value with a default fallback.
func GetFloat(s loom.State, key string, fallback float64) float64 {
	if v, ok := s[key].(float64); ok {
		return v
	}
	return fallback
}

// GetBool retrieves a bool value with a default fallback.
func GetBool(s loom.State, key string, fallback bool) bool {
	if v, ok := s[key].(bool); ok {
		return v
	}
	return fallback
}

// SetOutput is the type-safe setter for agent output.
func SetOutput(content string, usage contract.Usage) loom.State {
	return loom.State{"output": content, "usage": usage}
}
