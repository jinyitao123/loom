package stdlib

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropic/loom/contract"
)

// PermissionDispatcher wraps a ToolDispatcher with declarative deny/allow rules.
// Evaluation order: deny (checked first, not overridable) → allow (whitelist).
type PermissionDispatcher struct {
	inner contract.ToolDispatcher
	deny  []string
	allow []string
}

// NewPermissionDispatcher creates a permission-aware tool dispatcher.
//   - deny: tools matching these patterns are always blocked (even if in allow list).
//   - allow: if non-empty, only tools matching these patterns are permitted.
func NewPermissionDispatcher(inner contract.ToolDispatcher, deny, allow []string) *PermissionDispatcher {
	return &PermissionDispatcher{
		inner: inner,
		deny:  deny,
		allow: allow,
	}
}

// ListTools returns only permitted tools (filters out denied, non-allowed).
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
		if len(p.allow) > 0 && !matchAny(p.allow, t.Name) {
			continue
		}
		visible = append(visible, t)
	}
	return visible, nil
}

// Dispatch executes a tool call if permitted.
func (p *PermissionDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if matchAny(p.deny, call.Name) {
		return nil, fmt.Errorf("loom/permission: tool %q denied by policy", call.Name)
	}
	if len(p.allow) > 0 && !matchAny(p.allow, call.Name) {
		return nil, fmt.Errorf("loom/permission: tool %q not in allow list", call.Name)
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
