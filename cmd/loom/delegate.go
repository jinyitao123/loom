// delegate.go —— 编排路径的委派机制。
// 模型通过合成的 delegate 工具"提议"转交：分发器把选择写进共享信号，
// chat 步骤包装器在收尾时将其转写为状态里的 __delegate_to 路由键，
// 编译器生成的确定性 BranchFunc 据此查表路由到对应的 sub_<name> 子图。

package main

import (
	"context"       // 工具调用与工具循环的上下文传播
	"encoding/json" // delegate 工具入参解析与 schema 序列化
	"fmt"           // 工具描述与错误文案的拼装
	"strings"       // 子 agent 清单文案的多行拼接
	"sync"          // 委派信号的并发读写保护

	"github.com/jinyitao123/loom"          // 状态/步骤等编排原语
	"github.com/jinyitao123/loom/contract" // LLM 与工具契约类型
	"github.com/jinyitao123/loom/stdlib"   // 工具循环步骤与 SubAgentRef 定义
)

// delegateToolName is the synthetic tool the model calls to hand this turn off
// to a specialist sub-agent.
// delegateToolName 是暴露给模型的合成工具名：模型调用它即表示"把本轮转交给某个子 agent"。
const delegateToolName = "delegate"

// delegateSignal records the sub-agent a delegate tool call chose. It is shared
// between the delegateDispatcher (which captures the call) and the chat-step
// wrapper (which turns the choice into a router signal). A tool result cannot
// write graph state, so this closure-shared struct is how the model's routing
// decision reaches the deterministic router. Mutex-guarded because read-only
// tool calls can dispatch in parallel.
// delegateSignal 是委派决定的旁路信道：工具结果写不进图状态，于是让分发器（写端）
// 与 chat 步骤包装器（读端）共享这枚结构，把模型的路由选择带给确定性路由器。
// 加互斥锁是因为只读工具调用可能并行分发。
type delegateSignal struct {
	mu    sync.Mutex // 保护以下三个字段的读写
	set   bool       // 是否已发生委派（区分"未委派"与"委派目标恰为空串"）
	agent string     // 被选中的子 agent 路由键
	task  string     // 转交给子 agent 的任务指令
}

// record 记录一次委派选择（写端：delegate 工具被调用时触发）。
func (s *delegateSignal) record(agent, task string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set = true // 置位后 chat 步骤包装器即可感知本轮发生了委派
	s.agent = agent
	s.task = task
}

// read 原子读取当前委派状态（读端：工具循环结束后由包装器查询）。
func (s *delegateSignal) read() (set bool, agent, task string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.set, s.agent, s.task
}

// delegateDispatcher wraps the real tool dispatcher and adds one synthetic
// `delegate` tool. When the model calls it, the chosen sub-agent is recorded in
// the shared signal and a benign result is returned; every other tool passes
// through to the inner dispatcher.
// delegateDispatcher 是真实分发器的装饰器：在原有工具集之上附加一个合成的 delegate
// 工具。模型调用 delegate 时只记录信号并返回无害结果（并不真正"执行"什么）；
// 其余工具调用原样透传给内层分发器。
type delegateDispatcher struct {
	inner contract.ToolDispatcher // 被包装的真实分发器（可为 nil：除 delegate 外无任何工具）
	subs  []stdlib.SubAgentRef    // spec 声明的子 agent 列表，用于生成枚举与合法性校验
	sig   *delegateSignal         // 与 chat 步骤包装器共享的委派信号
}

// newDelegateDispatcher 组装装饰器；三个协作者均由编排路径（runOrchestrated）注入。
func newDelegateDispatcher(inner contract.ToolDispatcher, subs []stdlib.SubAgentRef, sig *delegateSignal) *delegateDispatcher {
	return &delegateDispatcher{inner: inner, subs: subs, sig: sig}
}

// routeKey returns the value the router branches on for a sub-agent (RouteKey,
// defaulting to Name).
// routeKey 求某个子 agent 在路由器上的分支键：显式 RouteKey 优先，缺省回落到 Name。
// 该键必须与编译器生成的 sub_<key> 子图节点名保持一致，否则路由无法命中。
func routeKey(sa stdlib.SubAgentRef) string {
	if sa.RouteKey != "" {
		return sa.RouteKey
	}
	return sa.Name
}

// ListTools 返回"内层全部工具 + 合成 delegate 工具"，让模型在同一工具面板上
// 既能自己干活也能选择转交。
func (d *delegateDispatcher) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	var tools []contract.ToolDef
	if d.inner != nil { // 先取内层真实工具；内层失败则整体失败（清单必须完整可信）
		inner, err := d.inner.ListTools(ctx)
		if err != nil {
			return nil, err
		}
		tools = append(tools, inner...)
	}
	tools = append(tools, d.delegateToolDef()) // 末尾追加合成的 delegate 工具
	return tools, nil
}

// delegateToolDef 动态生成 delegate 工具的定义：agent 参数用 enum 锁死在已声明的
// 子 agent 路由键上（从 schema 层面阻止模型编造目标），描述里逐条列出各子 agent
// 的用途，帮助模型做出正确的转交判断。
func (d *delegateDispatcher) delegateToolDef() contract.ToolDef {
	names := make([]string, 0, len(d.subs)) // enum 取值集合：全部合法路由键
	lines := make([]string, 0, len(d.subs)) // 描述行：`- 键: 说明`
	for _, sa := range d.subs {
		key := routeKey(sa)
		names = append(names, key)
		desc := sa.Description
		if desc == "" { // 未写描述时退化为用名字自述，保证每行都有内容
			desc = sa.Name
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", key, desc))
	}
	// JSON Schema：agent（枚举，选谁）与 task（自由文本，交代什么）均为必填。
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"agent": map[string]any{"type": "string", "enum": names, "description": "which sub-agent to hand off to"},
			"task":  map[string]any{"type": "string", "description": "the task / instruction for the sub-agent"},
		},
		"required": []string{"agent", "task"},
	}
	raw, _ := json.Marshal(schema) // 纯字面量结构的序列化不会失败，忽略错误是安全的
	return contract.ToolDef{
		Name:        delegateToolName,
		Description: "Hand this turn off to a specialist sub-agent when the request matches one of them. Sub-agents:\n" + strings.Join(lines, "\n"),
		InputSchema: raw,
	}
}

// Dispatch 拦截 delegate 调用并记录委派信号；其余调用透传内层。
// 注意 delegate 侧的所有失败都以 IsError 结果回给模型（而非 Go 错误），
// 让模型能读到原因并自行修正重试。
func (d *delegateDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if call.Name != delegateToolName { // 非 delegate：走真实工具路径
		if d.inner != nil {
			return d.inner.Dispatch(ctx, call)
		}
		// 无内层却被调了别的工具：以错误结果兜底，不让整轮崩溃。
		return &contract.ToolResult{CallID: call.ID, Content: "no tools configured", IsError: true}, nil
	}
	// 解析 delegate 的两个入参：agent（转交给谁）与 task（交代什么任务）。
	var args struct {
		Agent string `json:"agent"`
		Task  string `json:"task"`
	}
	if err := json.Unmarshal([]byte(call.Args), &args); err != nil { // 入参不是合法 JSON：回错误结果，让模型修正后重试
		return &contract.ToolResult{CallID: call.ID, Content: "delegate: invalid args", IsError: true}, nil
	}
	if !d.known(args.Agent) { // schema enum 之外的兜底校验：目标必须是已声明的子 agent
		return &contract.ToolResult{CallID: call.ID, Content: fmt.Sprintf("delegate: unknown sub-agent %q", args.Agent), IsError: true}, nil
	}
	d.sig.record(args.Agent, args.Task) // 关键动作：把委派选择写进共享信号，等 chat 步骤收尾时转为路由键
	// 返回无害的确认文本：工具循环看到普通成功结果即可自然收敛。
	return &contract.ToolResult{CallID: call.ID, Content: "delegating to " + args.Agent}, nil
}

// known 校验目标名是否命中任一已声明子 agent（路由键或原名皆算命中）。
func (d *delegateDispatcher) known(name string) bool {
	for _, sa := range d.subs { // 子 agent 数量很小，线性扫描足矣
		if routeKey(sa) == name || sa.Name == name {
			return true
		}
	}
	return false
}

// delegateChatStepFactory builds the orchestrator's chat step: run the normal
// tool loop, then — if the model called delegate — return a state carrying
// __delegate_to (and the delegated task as last_user_message) so the compiler's
// deterministic router branches to sub_<agent>. Otherwise return the tool loop's
// own delta (the answer). Wire it via compiler.CompileOpts.ChatStepFactory.
// delegateChatStepFactory 生成编排器的 chat 步骤工厂：内核仍是标准工具循环，
// 只在收尾处查询委派信号 —— 若模型调用过 delegate，就返回携带 __delegate_to 的
// 状态增量，编译出的确定性 BranchFunc 据此查表跳到 sub_<agent> 子图；
// 否则原样返回工具循环自己的增量（即最终回答）。经 CompileOpts.ChatStepFactory 接线。
func delegateChatStepFactory(sig *delegateSignal) func(contract.LLM, contract.ToolDispatcher, stdlib.ToolLoopOpts) loom.Step {
	// 外层函数即工厂：编译器建图时以 (llm, tools, opts) 调用它换取步骤实例。
	return func(llm contract.LLM, tools contract.ToolDispatcher, opts stdlib.ToolLoopOpts) loom.Step {
		inner := stdlib.NewToolLoopStep(llm, tools, opts) // 先构造被包装的标准工具循环步骤
		// 返回的闭包才是真正挂进图的 chat 步骤。
		return func(ctx context.Context, state loom.State) (loom.State, error) {
			delta, err := inner(ctx, state) // 完整跑一轮工具循环（期间 delegate 调用可能已写入信号）
			if err != nil {
				return delta, err // 工具循环失败：增量与错误原样上抛，不做委派判断
			}
			if set, agent, task := sig.read(); set { // 收尾查询信号：本轮是否发生了委派
				// Delegation is terminal for this turn: drop any pre-delegation
				// output — the sub-agent owns the answer.
				// 委派即本步骤的终点：丢弃委派前的任何 output，最终回答由子 agent 负责产出。
				next := loom.State{"__delegate_to": agent} // 写入路由协议键，路由器据此选中 sub_<agent> 分支
				if task != "" {                            // 委派附带的任务指令替换 last_user_message，成为子 agent 眼中的用户输入
					next["last_user_message"] = task
				}
				return next, nil
			}
			return delta, nil // 未委派：工具循环的增量（含 output）就是本轮答案
		}
	}
}
