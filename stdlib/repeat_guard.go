package stdlib

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"

	"github.com/jinyitao123/loom/contract"
)

// NewToolRepeatGuard returns a soft tool-loop guard. After maxRepeats
// consecutive executions with the same tool name and arguments, the next call
// is blocked and returned to the model as an error observation. This gives the
// agent another turn to choose a different action. ToolLoop's batch repeat
// limit remains the hard stop for a model that ignores that feedback.
func NewToolRepeatGuard(maxRepeats int) contract.ToolHook {
	if maxRepeats <= 0 {
		maxRepeats = 5
	}
	guard := &toolRepeatGuard{maxRepeats: maxRepeats, history: make([]string, maxRepeats)}
	return contract.ToolHook{
		Pre: func(_ context.Context, call contract.ToolCall) (contract.ToolCall, error) {
			signature := toolCallSignature(call)
			if guard.repeated(signature) {
				return call, fmt.Errorf("tool loop detected: %q called %d consecutive times with identical arguments; choose a different action", call.Name, maxRepeats)
			}
			return call, nil
		},
		Post: func(_ context.Context, call contract.ToolCall, _ *contract.ToolResult) error {
			guard.record(toolCallSignature(call))
			return nil
		},
	}
}

type toolRepeatGuard struct {
	mu         sync.Mutex
	maxRepeats int
	history    []string
	position   int
	size       int
}

func toolCallSignature(call contract.ToolCall) string {
	digest := sha256.Sum256([]byte(call.Name + ":" + call.Args))
	return fmt.Sprintf("%x", digest[:8])
}

func (guard *toolRepeatGuard) record(signature string) {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	guard.history[guard.position] = signature
	guard.position = (guard.position + 1) % len(guard.history)
	if guard.size < len(guard.history) {
		guard.size++
	}
}

func (guard *toolRepeatGuard) repeated(signature string) bool {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if guard.size < guard.maxRepeats {
		return false
	}
	for index := 0; index < guard.maxRepeats; index++ {
		position := (guard.position - 1 - index + len(guard.history)) % len(guard.history)
		if guard.history[position] != signature {
			return false
		}
	}
	return true
}
