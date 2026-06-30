package compiler

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"

	"github.com/jinyitao123/loom/contract"
)

// loopDetector tracks recent tool call signatures and blocks when the same
// call (name + args hash) repeats consecutively beyond a threshold. This
// prevents LLMs from getting stuck in retry loops. Ported from the archived
// weave compiler (zero platform coupling — depends only on contract).
type loopDetector struct {
	mu        sync.Mutex
	threshold int      // consecutive repeats before blocking
	history   []string // ring buffer of call signatures
	pos       int      // write position
	size      int      // current fill level
}

func newLoopDetector(threshold int) *loopDetector {
	if threshold <= 0 {
		threshold = 5
	}
	return &loopDetector{
		threshold: threshold,
		history:   make([]string, threshold+1), // track threshold+1 to detect threshold repeats
	}
}

// signature returns a stable hash of tool name + args.
func signature(call contract.ToolCall) string {
	h := sha256.Sum256([]byte(call.Name + ":" + call.Args))
	return fmt.Sprintf("%x", h[:8])
}

// record adds a call signature to the history.
func (d *loopDetector) record(sig string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.history[d.pos] = sig
	d.pos = (d.pos + 1) % len(d.history)
	if d.size < len(d.history) {
		d.size++
	}
}

// isLoop checks if the last `threshold` entries all match the given signature.
func (d *loopDetector) isLoop(sig string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.size < d.threshold {
		return false
	}
	for i := 0; i < d.threshold; i++ {
		idx := (d.pos - 1 - i + len(d.history)) % len(d.history)
		if d.history[idx] != sig {
			return false
		}
	}
	return true
}

// hook returns a ToolHook that detects and breaks tool call loops.
func (d *loopDetector) hook() contract.ToolHook {
	return contract.ToolHook{
		Pre: func(_ context.Context, call contract.ToolCall) (contract.ToolCall, error) {
			sig := signature(call)
			if d.isLoop(sig) {
				return call, fmt.Errorf("tool loop detected: %q called %d consecutive times with identical arguments — try a different approach", call.Name, d.threshold)
			}
			return call, nil
		},
		Post: func(_ context.Context, call contract.ToolCall, _ *contract.ToolResult) error {
			d.record(signature(call))
			return nil
		},
	}
}
