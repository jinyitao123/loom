package stdlib

import (
	"context" // 透传给内层派发器
	"fmt"     // 构造拒绝错误与确认提示 JSON
	"strings" // matchAny 的前缀/精确匹配

	"github.com/jinyitao123/loom/contract" // ToolDispatcher/ToolDef/ToolCall/ToolResult 抽象
)

// PermissionDispatcher wraps a ToolDispatcher with declarative deny/allow/ask rules.
// Evaluation order: deny (block) → ask (execute with confirmation hint) → allow (whitelist).
// PermissionDispatcher 以装饰器方式给任意 ToolDispatcher 叠加声明式权限规则。
// 三级评估顺序固定为 deny → ask → allow：deny 命中即无条件拒绝（优先级最高）；
// ask 命中则不执行、要求用户确认；allow 非空时充当白名单，未列入者一律拒绝。
type PermissionDispatcher struct {
	inner contract.ToolDispatcher // 被包装的真实派发器（本地工具、MCP 等）
	deny  []string                // 黑名单模式列表：命中即拒绝，任何情况下不放行
	allow []string                // 白名单模式列表：非空时只有命中者（含 ask 工具）可通过
	ask   []string                // tools that need user confirmation — executed but result includes confirmation hint
	// ask 是"需用户确认"级别的模式列表：命中的调用不会真正执行，
	// 而是返回 needs_confirmation 结果让模型先向用户复述、征得同意后重调。
}

// NewPermissionDispatcher creates a permission-aware tool dispatcher.
//   - deny: tools matching these patterns are always blocked.
//   - allow: if non-empty, only tools matching these patterns are permitted.
//
// NewPermissionDispatcher 构造两级（deny/allow）权限派发器：
// deny 命中的工具永远被拒；allow 非空时即启用白名单模式。
func NewPermissionDispatcher(inner contract.ToolDispatcher, deny, allow []string) *PermissionDispatcher {
	return &PermissionDispatcher{
		inner: inner,
		deny:  deny,
		allow: allow,
		// ask 留空：即退化为传统的黑白名单两级模型。
	}
}

// NewPermissionDispatcherWithAsk creates a dispatcher with three permission levels.
//   - deny: always blocked.
//   - ask: executed but result wrapped with confirmation hint for the LLM to relay to user.
//   - allow: if non-empty, only these are permitted (ask tools are auto-included).
//
// NewPermissionDispatcherWithAsk 构造完整三级（deny/ask/allow）权限派发器：
// ask 级工具自动视为"在白名单内"，无需在 allow 里重复声明。
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
// ListTools 只把有权限的工具暴露给模型：被 deny 或不在白名单内的工具直接不可见，
// 模型从源头上就不会尝试调用它们；ask 级工具保持可见（它们只是多一道确认）。
func (p *PermissionDispatcher) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	// 先取内层的完整工具清单。
	all, err := p.inner.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	// 按 deny → allow(含 ask 豁免) 顺序逐个过滤。
	var visible []contract.ToolDef
	for _, t := range all {
		if matchAny(p.deny, t.Name) {
			continue // deny 命中：直接隐藏，优先级最高
		}
		// 白名单已启用（allow 非空）且既不在 allow 也不在 ask 中：隐藏。
		if len(p.allow) > 0 && !matchAny(p.allow, t.Name) && !matchAny(p.ask, t.Name) {
			continue
		}
		visible = append(visible, t) // 通过全部检查，对模型可见
	}
	return visible, nil
}

// Dispatch executes a tool call if permitted.
// Ask-level tools are NOT executed — a special result with needs_confirmation is returned
// so the LLM asks the user for confirmation before the tool actually runs.
// Dispatch 在派发前做与 ListTools 一致的三级权限评估（防御模型仍强行调用不可见工具）：
// deny 命中报错；白名单外报错；ask 命中不执行、返回确认请求；其余放行给内层执行。
func (p *PermissionDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	// 第一级 deny：命中即以错误拒绝，模型会把该错误当作调用失败反馈。
	if matchAny(p.deny, call.Name) {
		return nil, fmt.Errorf("loom/permission: tool %q denied by policy", call.Name)
	}
	// 第三级 allow 的拦截判断提前于 ask 的执行分支：白名单启用时，
	// 不在 allow 且不在 ask（ask 自动豁免）的调用直接拒绝。
	if len(p.allow) > 0 && !matchAny(p.allow, call.Name) && !matchAny(p.ask, call.Name) {
		return nil, fmt.Errorf("loom/permission: tool %q not in allow list", call.Name)
	}

	// Ask-level: do NOT execute. Return a confirmation request.
	// 第二级 ask：工具不执行，而是返回一个 IsError=false 的"伪结果"，
	// 内容是 needs_confirmation 协议 JSON——模型读到后应向用户复述将执行的操作与参数，
	// 等用户明确同意后再次发起同样的调用（届时由宿主调整规则放行）。
	if matchAny(p.ask, call.Name) {
		return &contract.ToolResult{
			CallID:   call.ID,   // 回填调用 ID，让确认请求正确挂到本次调用上
			ToolName: call.Name, // 冗余工具名，供 Post 钩子匹配
			Content:  fmt.Sprintf(`{"needs_confirmation":true,"tool":"%s","args":%s,"message":"此操作需要用户确认后才能执行。请向用户说明你打算执行的操作及参数，等待用户明确同意后再次调用。"}`, call.Name, call.Args),
			IsError:  false, // 不是错误：这是正常的协议响应，避免模型误判工具坏了
		}, nil
	}

	// 全部检查通过：真正派发给内层执行。
	return p.inner.Dispatch(ctx, call)
}

// matchAny checks if name matches any of the patterns.
// Supports simple prefix matching with "*" suffix (e.g. "Bash(rm *)" matches "Bash(rm -rf /)").
// matchAny 判断工具名是否命中任一模式。规则刻意保持极简：
// 模式以 "*" 结尾时按去掉 "*" 后的前缀匹配（如 "Bash(rm *" 前缀可覆盖一族命令），
// 否则要求全名精确相等；不支持通配符出现在中间等复杂语法。
func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "*") {
			// 前缀模式：去掉尾部 "*" 得到前缀，再做前缀比较。
			prefix := strings.TrimSuffix(p, "*")
			if strings.HasPrefix(name, prefix) {
				return true
			}
		} else if p == name {
			// 精确模式：全名完全一致才算命中。
			return true
		}
	}
	// 所有模式都未命中。
	return false
}

// Compile-time interface check.
// 编译期断言：确保 PermissionDispatcher 始终完整实现 ToolDispatcher 接口，
// 接口一旦变动会在编译期而非运行期暴露。
var _ contract.ToolDispatcher = (*PermissionDispatcher)(nil)
