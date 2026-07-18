// Skill activation mechanism S1 moves selection from the matcher to the model.
// The matcher becomes an optional host-side preload hint; weave wiring handles that later, outside this repository.
// 技能激活机制 S1——选择权从 matcher 移交模型；matcher 降级为宿主侧可选预加载提示，
// weave 接线阶段处理，不在本仓。
package stdlib

import (
	"context"       // 透传给内层派发器
	"encoding/json" // 解析模型传入的 load_skill 参数并声明输入 schema
	"fmt"           // 组装技能正文与可纠正的错误结果
	"strings"       // 拼接未命中时的可用技能名

	"github.com/jinyitao123/loom/contract" // ToolDispatcher/ToolDef/ToolCall/ToolResult 抽象
)

const (
	loadSkillToolName        = "load_skill"
	loadSkillToolDescription = "The skill index in the system prompt lists available skills. When the current task is relevant to a skill, call this tool with its name to load the full body before continuing. " +
		"系统提示词里的技能索引列出了可用技能；当当前任务与某技能相关时，调用本工具按 name 取其完整正文再继续。"
)

// skillDispatcher adds the built-in load_skill tool without changing the wrapped dispatcher.
// skillDispatcher 以装饰器方式追加内建 load_skill；技能快照在构造后只读，可被工具循环并发调用。
type skillDispatcher struct {
	inner      contract.ToolDispatcher // 被包装的真实派发器，其余工具调用完整透传
	skills     map[string]SkillDef     // 技能名到定义的纯内存只读索引
	skillNames []string                // 保留构造顺序，未命中时帮助模型自纠
}

// NewSkillDispatcher wraps a ToolDispatcher, adding the built-in read-only load_skill tool.
// NewSkillDispatcher 为任意工具派发器追加只读 load_skill；零技能时原样返回 inner，保持零开销。
func NewSkillDispatcher(inner contract.ToolDispatcher, skills []SkillDef) contract.ToolDispatcher {
	if len(skills) == 0 {
		return inner
	}

	// 只快照工具实际读取的三个字段，避免调用方后续改写原切片影响已构造的装饰器。
	byName := make(map[string]SkillDef, len(skills))
	names := make([]string, 0, len(skills))
	for _, skill := range skills {
		byName[skill.Name] = SkillDef{
			Name:        skill.Name,
			Description: skill.Description,
			Body:        skill.Body,
		}
		names = append(names, skill.Name)
	}

	return &skillDispatcher{inner: inner, skills: byName, skillNames: names}
}

// ListTools returns all inner tools plus the built-in read-only load_skill definition.
// ListTools 返回内层工具并在末尾追加 load_skill；内建工具名由装饰器优先，碰撞时覆盖内层定义。
func (d *skillDispatcher) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	innerTools, err := d.inner.ListTools(ctx)
	if err != nil {
		return nil, err
	}

	// Built-in tool names take decorator precedence.
	// 内建工具名装饰器优先：过滤内层同名项，保证模型只看到一个权威定义。
	tools := make([]contract.ToolDef, 0, len(innerTools)+1)
	for _, tool := range innerTools {
		if tool.Name != loadSkillToolName {
			tools = append(tools, tool)
		}
	}
	tools = append(tools, contract.ToolDef{
		Name:        loadSkillToolName,
		Description: loadSkillToolDescription,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"skill name from the index"}},"required":["name"]}`),
		ReadOnly:    true,
	})
	return tools, nil
}

// Dispatch serves load_skill from the immutable in-memory index and delegates every other call unchanged.
// Dispatch 只截获 load_skill；命中、未命中与参数错误都返回 ToolResult，避免中断整个工具循环。
func (d *skillDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if call.Name != loadSkillToolName {
		return d.inner.Dispatch(ctx, call)
	}

	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
		return skillToolError(call.ID, fmt.Sprintf(
			`invalid load_skill arguments: expected JSON in the form {"name":"<skill name from the index>"}: %v`, err,
		)), nil
	}

	skill, ok := d.skills[args.Name]
	if !ok {
		return skillToolError(call.ID, fmt.Sprintf(
			"skill %q was not found; available skills: %s", args.Name, strings.Join(d.skillNames, ", "),
		)), nil
	}

	return &contract.ToolResult{
		CallID:   call.ID,
		ToolName: loadSkillToolName,
		Content:  fmt.Sprintf("# skill: %s\n%s\n\n%s", skill.Name, skill.Description, skill.Body),
	}, nil
}

// skillToolError encodes recoverable model mistakes as tool results rather than runtime errors.
// skillToolError 把可自纠的模型调用错误编码为 IsError 结果，并保留 call_id 供消息配对。
func skillToolError(callID, content string) *contract.ToolResult {
	return &contract.ToolResult{
		CallID:   callID,
		ToolName: loadSkillToolName,
		Content:  content,
		IsError:  true,
	}
}

// 编译期断言：接口若发生变化，在构建阶段立即暴露。
var _ contract.ToolDispatcher = (*skillDispatcher)(nil)
