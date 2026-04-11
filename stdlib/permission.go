package stdlib

import (
	"context"
	"fmt"
	"strings"

	"github.com/jinyitao123/loom/contract"
)

// PermissionDispatcher wraps a ToolDispatcher with declarative deny/allow/ask rules.
// Evaluation order: deny (block) → ask (execute with confirmation hint) → allow (whitelist).
type PermissionDispatcher struct {
	inner contract.ToolDispatcher
	deny  []string
	allow []string
	ask   []string // tools that need user confirmation — executed but result includes confirmation hint
}

// NewPermissionDispatcher creates a permission-aware tool dispatcher.
//   - deny: tools matching these patterns are always blocked.
//   - allow: if non-empty, only tools matching these patterns are permitted.
func NewPermissionDispatcher(inner contract.ToolDispatcher, deny, allow []string) *PermissionDispatcher {
	return &PermissionDispatcher{
		inner: inner,
		deny:  deny,
		allow: allow,
	}
}

// NewPermissionDispatcherWithAsk creates a dispatcher with three permission levels.
//   - deny: always blocked.
//   - ask: executed but result wrapped with confirmation hint for the LLM to relay to user.
//   - allow: if non-empty, only these are permitted (ask tools are auto-included).
func NewPermissionDispatcherWithAsk(inner contract.ToolDispatcher, deny, ask, allow []string) *PermissionDispatcher {
	return &PermissionDispatcher{
		inner: inner,
		deny:  deny,
		allow: allow,
		ask:   ask,
	}
}

// ListTools returns only permitted tools (filters out denied, non-allowed).
// Ask-level tools are visible (they execute, just with a confirmation wrapper).
func (p *PermissionDispatcher) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	all, err := p.inner.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	var visible []contract.ToolDef
	for _, t := range all {
		if matchAny(p.deny, t.Name) {
			continue
		}
		if len(p.allow) > 0 && !matchAny(p.allow, t.Name) && !matchAny(p.ask, t.Name) {
			continue
		}
		visible = append(visible, t)
	}
	return visible, nil
}

// Dispatch executes a tool call if permitted.
// Ask-level tools are NOT executed — a special result with needs_confirmation is returned
// so the LLM asks the user for confirmation before the tool actually runs.
func (p *PermissionDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if matchAny(p.deny, call.Name) {
		return nil, fmt.Errorf("loom/permission: tool %q denied by policy", call.Name)
	}
	if len(p.allow) > 0 && !matchAny(p.allow, call.Name) && !matchAny(p.ask, call.Name) {
		return nil, fmt.Errorf("loom/permission: tool %q not in allow list", call.Name)
	}

	// Ask-level: do NOT execute. Return a confirmation request.
	if matchAny(p.ask, call.Name) {
		return &contract.ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Content:  fmt.Sprintf(`{"needs_confirmation":true,"tool":"%s","args":%s,"message":"此操作需要用户确认后才能执行。请向用户说明你打算执行的操作及参数，等待用户明确同意后再次调用。"}`, call.Name, call.Args),
			IsError:  false,
		}, nil
	}

	return p.inner.Dispatch(ctx, call)
}

// matchAny checks if name matches any of the patterns.
// Supports simple prefix matching with "*" suffix (e.g. "Bash(rm *)" matches "Bash(rm -rf /)").
func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "*") {
			prefix := strings.TrimSuffix(p, "*")
			if strings.HasPrefix(name, prefix) {
				return true
			}
		} else if p == name {
			return true
		}
	}
	return false
}

// Compile-time interface check.
var _ contract.ToolDispatcher = (*PermissionDispatcher)(nil)
