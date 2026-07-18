// Package compiler turns a stdlib.AgentSpec into a runnable loom.Graph for
// deterministic orchestration: a parent agent routes to named sub-agents by
// writing a RouteKey into state, instead of relying on the model to "decide the
// next step" in a flat tool loop.
//
// This is the lean, spec-driven port of the archived weave compiler. It
// deliberately sheds the platform-shell coupling (registry.AgentRecord) and the
// orthogonal extras that the loom consolidation cut or deferred — memory
// retrieval / auto-remember (replaced by weave's slot system), otel
// trace/audit, llmrouter fallback, guards, compaction, semantic (embedder)
// skill matching. What remains is the orchestration topology + deterministic
// router. See plans/loom-p3-orchestration.md.
//
// 中文说明：本包把声明式的 stdlib.AgentSpec 编译成可运行的 loom.Graph（图），
// 实现"确定性子 agent 编排"。核心契约是：模型或工具只拥有"提议权"——向状态
// （State）写入 __delegate_to；而"提议如何生效"由代码裁决——路由器（Router）
// 对预先注册的路由表做确定性查表，命中则进入对应子 agent 步骤，查不到则正常
// 结束。可编排的目标集合在编译期即被固定，模型无法路由到未声明的步骤，
// 这就是"确定性编排"区别于"扁平工具循环里让模型自由决定下一步"的地方。
// 本移植版刻意砍掉了与平台外壳耦合的部分和正交扩展（记忆检索、otel 追踪、
// llmrouter 回退、守卫、压缩、语义技能匹配等），只保留编排拓扑 + 确定性路由器。
package compiler

import (
	"fmt" // 错误包装与格式化

	"github.com/jinyitao123/loom"          // 内核五原语：State/Step/Router/Store/Graph
	"github.com/jinyitao123/loom/contract" // LLM / 工具调用的稳定契约类型（与具体实现解耦）
	"github.com/jinyitao123/loom/stdlib"   // 标准库步骤：prompt 组装、工具循环、子图封装等
)

// CompileOpts carries the runtime parameters the spec does not (the LLM/model,
// tools, and the resolver that turns a sub-agent name into a compiled child
// graph). All fields are optional except SubAgentResolver when the spec
// declares sub-agents.
//
// 中文说明：CompileOpts 承载 spec 里不包含的"运行时参数"（模型、工具钩子、
// 子 agent 解析器等）。除"spec 声明了子 agent 时必须提供 SubAgentResolver"外，
// 所有字段均可选，缺省值在 CompileAgent 内统一填充。
type CompileOpts struct {
	// 图的显示名，仅用于可视化/日志标识，缺省 "agent"。
	Name string // graph display name (default "agent")
	// 传给 LLM 的模型 id，缺省 "deepseek-chat"。
	Model string // model id (default "deepseek-chat")
	// 当前激活的 profile 名，供 prompt 组装步骤从 spec.Profiles 里选取对应人格/配置。
	Profile string // active profile name
	// 推理努力档位（映射为 reasoning effort），缺省 EffortMedium。
	Effort contract.EffortLevel // default EffortMedium
	// 运行时上下文键值对，由 prompt 组装步骤渲染进 system prompt 的"## 当前上下文"小节。
	Context map[string]any // runtime context → "## 当前上下文"
	// 宿主注入的工具前/后置钩子，按序挂到 chat 步骤的工具循环上。
	ToolHooks []contract.ToolHook // injected tool pre/post hooks
	// 大于 0 时在 chat 步骤上追加一个防重复护栏（loopDetector）：
	// 同一工具以完全相同参数连续调用达到该次数即被拦截。
	MaxToolRepeats int // >0 wires a loop detector on the chat step
	// 存储（Store），供子图步骤做检查点持久化；无子 agent 时可为空。
	Store loom.Store // for sub-graph checkpointing
	// 子 agent 解析器：把子 agent 名解析为已编译好的子图。
	// spec 声明了子 agent 但未提供解析器时，编排拓扑不会启用（静默退化为单 agent）。
	SubAgentResolver func(name string) (*loom.Graph, error) // resolve a sub-agent name → child graph
	// ChatStepFactory, when non-nil, builds the "chat" step instead of the
	// default stdlib.NewToolLoopStep — letting a caller wrap the tool loop (e.g.
	// to capture a delegation signal and surface it as state["__delegate_to"] so
	// the deterministic router fires). nil = identical to the default build.
	//
	// 中文说明：这是留给宿主的扩展点——用自定义工厂替换缺省的工具循环步骤，
	// 典型用途是"包装"工具循环：捕获委派信号并把它写成 state["__delegate_to"]，
	// 从而触发确定性路由器。为 nil 时行为与缺省构建完全一致。
	ChatStepFactory func(llm contract.LLM, tools contract.ToolDispatcher, opts stdlib.ToolLoopOpts) loom.Step
}

// CompileAgent converts an AgentSpec into a runnable loom Graph.
//
//	No sub-agents (default):
//	    prompt_assemble → chat → END
//	Orchestrator (sub_agents declared + resolver provided):
//	    prompt_assemble → chat → route → sub_<name> → END
//
// The route is a deterministic Branch: the chat step (or a tool) writes
// __delegate_to into state, and the router maps that value (or a sub-agent's
// RouteKey) to the matching sub_<name> step.
//
// 中文说明：CompileAgent 把 AgentSpec 编译为可运行的图，只有两种拓扑：
//   - 无子 agent（缺省）：prompt_assemble → chat → END，纯单 agent 对话；
//   - 编排者（声明了 sub_agents 且提供了解析器）：chat 之后接一个确定性
//     分支路由——chat 步骤（或某个工具）向状态写 __delegate_to，路由器查表
//     命中则进入对应 sub_<name> 子图步骤，查不到则直接结束。
func CompileAgent(spec *stdlib.AgentSpec, llm contract.LLM, tools contract.ToolDispatcher, opts CompileOpts) (*loom.Graph, error) {
	// 防御性校验：spec 是编译器唯一不可缺省的输入，为 nil 直接报错而非 panic。
	if spec == nil {
		return nil, fmt.Errorf("compiler: nil AgentSpec")
	}
	// 以下三段为可选运行时参数填缺省值：模型、努力档位、图名。
	model := opts.Model
	if model == "" {
		model = "deepseek-chat" // 模型 id 缺省 deepseek-chat
	}
	effort := opts.Effort
	if effort == "" {
		effort = contract.EffortMedium // 推理努力档位缺省中档
	}
	name := opts.Name
	if name == "" {
		name = "agent" // 图显示名缺省 "agent"
	}

	// 判定编排形态：必须同时满足"spec 声明了子 agent"和"提供了解析器"
	// 才启用编排拓扑；缺一即静默退化为单 agent 拓扑（不报错，保持兼容）。
	hasSubAgents := len(spec.SubAgents) > 0 && opts.SubAgentResolver != nil

	// WithMaxIterations(50) 是图级熔断阈值：无论路由怎么写，步骤执行总数
	// 达到 50 次即强制终止，兜底防御路由配置错误或委派互相踢皮球造成的死循环。
	graphOpts := []loom.GraphOption{loom.WithMaxIterations(50)}
	// MergeConfig so messages accumulate correctly across sub-graphs.
	// 仅在有子 agent 时才挂消息累积的合并配置：子图执行后其状态要并回父图，
	// messages 这类会话历史必须"追加"而不是"覆盖"，否则父图的对话记录会被
	// 子图返回的状态冲掉。无子 agent 时不存在跨图状态合并，无需此配置。
	if hasSubAgents {
		graphOpts = append(graphOpts, loom.WithMergeConfig(loom.DefaultMergeConfig()))
	}

	// 创建图：入口步骤固定为 prompt_assemble，即每次运行都先组装 system prompt。
	g := loom.NewGraph(name, "prompt_assemble", graphOpts...)

	// Identity: SystemPrompt (v1.2 compat) folds into core; always-active skill
	// bodies are injected directly; the rest go through the prompt assembler's
	// matcher (KeywordMatcher — semantic/embedder matching is not ported).
	//
	// 中文说明：身份组装分三层——
	//  1) v1.2 兼容：老 spec 只有 SystemPrompt 而没有结构化 Identity 时，
	//     把 SystemPrompt 折叠为身份核心；
	//  2) AlwaysActive 技能正文直接拼入身份核心，无条件常驻每轮 prompt；
	//  3) 其余技能交给 prompt 组装步骤的 KeywordMatcher 按需匹配注入
	//     （语义/向量匹配未被移植，这里只有关键词匹配）。
	identity := spec.Identity // 取值拷贝，后续拼接不会写回 spec
	// 兼容分支：仅当结构化身份核心为空且旧字段 SystemPrompt 非空时才折叠，
	// 二者都有时以结构化 Identity 为准。
	if identity.Core == "" && spec.SystemPrompt != "" {
		identity.Core = spec.SystemPrompt
	}
	// 技能分流：常驻技能进身份核心，可匹配技能收集到 matchableSkills。
	var matchableSkills []stdlib.SkillDef
	for _, s := range spec.Skills {
		if s.AlwaysActive && s.Body != "" {
			// 常驻技能：以固定小节格式（标题 + 强制遵守声明 + 正文）拼进身份核心，
			// 成为每轮 system prompt 的一部分，不经过任何匹配器。
			identity.Core += "\n\n## 技能: " + s.Name + "\n以下规则必须遵守：\n" + s.Body
		} else {
			// 非常驻技能（含正文为空的常驻技能）：延迟到运行时由匹配器
			// 依据当前输入决定是否注入，避免 prompt 无差别膨胀。
			matchableSkills = append(matchableSkills, s)
		}
	}

	// --- Step: prompt assembly → chat ---
	// 步骤一：prompt 组装。把身份、可匹配技能、profile、运行时上下文汇总成
	// system prompt 并写入状态；路由用 Always("chat")——无条件流向 chat 步骤。
	g.AddStep("prompt_assemble", stdlib.NewPromptAssembleStep(stdlib.PromptConfig{
		Identity:              identity,                 // 上面组装好的身份（含常驻技能）
		Skills:                matchableSkills,          // 待匹配的技能池
		Profile:               opts.Profile,             // 当前激活的 profile 名
		ProfilesMap:           spec.Profiles,            // spec 里声明的全部 profile
		Context:               opts.Context,             // 运行时上下文 → "## 当前上下文"
		MaxSystemPromptTokens: 8000,                     // system prompt 的 token 预算上限，超出时组装器裁剪
		SkillMatcher:          &stdlib.KeywordMatcher{}, // 关键词匹配器（语义匹配未移植）
	}), loom.Always("chat"))

	// --- Step: chat (tool loop) ---
	// 步骤二：chat（工具循环）。先组装钩子链，再决定路由器，最后构建步骤本体。
	// 拷贝一份宿主注入的钩子切片，避免 append 触碰调用方的底层数组。
	toolHooks := append([]contract.ToolHook{}, opts.ToolHooks...)
	// 按需追加防重复护栏：同一工具带相同参数连续调用达阈值即拦截（见 loop_detector.go）。
	if opts.MaxToolRepeats > 0 {
		toolHooks = append(toolHooks, newLoopDetector(opts.MaxToolRepeats).hook())
	}
	// chat 的缺省路由是 End()——单 agent 拓扑下对话结束即整图结束。
	chatRouter := loom.End()
	// 编排形态下换成确定性分支路由器：对 __delegate_to 查表决定去哪个子 agent。
	if hasSubAgents {
		chatRouter = buildSubAgentRouter(spec.SubAgents)
	}
	// 工具循环参数：模型、system prompt（循环内兜底用）、单轮最多 20 次工具
	// 迭代（这是 chat 步骤内部的预算，区别于上面 50 次的图级熔断）、努力档位、钩子链。
	chatOpts := stdlib.ToolLoopOpts{
		Model:         model,
		SystemPrompt:  spec.SystemPrompt,
		MaxIterations: 20,
		Effort:        effort,
		ToolHooks:     toolHooks,
	}
	// 构建 chat 步骤本体：宿主提供了工厂就用工厂（可包装工具循环、捕获委派信号），
	// 否则用标准库缺省的工具循环步骤，两者接到完全相同的参数。
	var chatStep loom.Step
	if opts.ChatStepFactory != nil {
		chatStep = opts.ChatStepFactory(llm, tools, chatOpts)
	} else {
		chatStep = stdlib.NewToolLoopStep(llm, tools, chatOpts)
	}
	// 注册 chat 步骤及其路由器（End 或子 agent 分支）。
	g.AddStep("chat", chatStep, chatRouter)

	// --- Steps: sub-agent delegation (optional) ---
	// 步骤三（可选）：为每个声明的子 agent 注册一个 sub_<name> 子图步骤。
	if hasSubAgents {
		for _, sa := range spec.SubAgents {
			// 编译期即解析子 agent 名→子图；解析失败让整个编译失败，
			// 保证运行时路由表里的每个目标都真实可达（fail fast）。
			child, err := opts.SubAgentResolver(sa.Name)
			if err != nil {
				return nil, fmt.Errorf("sub-agent %q: %w", sa.Name, err)
			}
			// 子图步骤：把子图包成一个步骤，检查点写入 opts.Store；
			// YieldBubble 表示子图内的中断/让出（yield）向上冒泡给父图处理。
			// 路由固定为 End()——子 agent 执行完即整图结束，不回到父 chat。
			g.AddStep("sub_"+sa.Name, stdlib.NewSubGraphStep(child, opts.Store, stdlib.SubGraphOpts{
				YieldPolicy: stdlib.YieldBubble,
			}), loom.End())
		}
	}

	// 导出图的静态形状（拓扑声明），供宿主做可视化/自省；不影响执行语义。
	g.SetTopology(buildTopology(model, spec.SubAgents, hasSubAgents))
	return g, nil // 编译完成，返回可运行的图
}

// buildSubAgentRouter creates a deterministic Branch router. The chat step (or a
// tool) signals delegation by writing __delegate_to into state; the router maps
// it to the matching sub_<name> step. A sub-agent's RouteKey (default: its Name)
// and its raw step name are both accepted.
//
// 中文说明：构建确定性分支路由器。委派协议是——chat 步骤（或工具）把提议
// 写进 state["__delegate_to"]，路由器只做一次纯查表：命中路由表则转到对应
// sub_<name> 步骤，否则结束。路由表在编译期封闭，模型写任何表外的值都只会
// 导致"正常结束"而非任意跳转——这是确定性的关键保证。
// 每个子 agent 接受两种键：RouteKey（缺省为其 Name）和原始步骤名 sub_<name>，
// 后者容错模型直接回吐步骤名的情况。
func buildSubAgentRouter(subAgents []stdlib.SubAgentRef) loom.Router {
	// 路由表：委派键 → 目标步骤名。
	routes := make(map[string]string)
	for _, sa := range subAgents {
		key := sa.RouteKey
		// RouteKey 缺省回退为子 agent 的 Name。
		if key == "" {
			key = sa.Name
		}
		stepName := "sub_" + sa.Name // 子图步骤的注册名
		routes[key] = stepName       // 主键：RouteKey（或 Name）→ 步骤
		// 同时接受原始步骤名 sub_<name> 作为别名键；相等时跳过避免自映射冗余。
		if key != stepName {
			routes[stepName] = stepName
		}
	}
	// BranchFunc：第一个参数从状态提取分支键，第二个是路由表，第三个是查不到时的回退。
	return loom.BranchFunc(
		func(s loom.State) string {
			// 只认字符串类型且非空的 __delegate_to；类型不符或缺失都视为"无委派"。
			if delegate, ok := s["__delegate_to"].(string); ok && delegate != "" {
				return delegate
			}
			return "" // 无委派信号 → 走回退分支
		},
		routes,
		"", // fallback: end graph (no delegation)
		// 回退为空串即结束整图：没有委派（或键不在表内）时对话正常收尾。
	)
}

// buildTopology declares the graph shape for visualization / introspection.
//
// 中文说明：静态声明图的形状（步骤 + 边），供宿主渲染拓扑图或做自省。
// 这里的 "router" 是给可视化用的逻辑节点——运行时路由器附着在 chat 步骤上，
// 并不是一个真实注册的步骤；边上的空 To 表示 END。
func buildTopology(model string, subAgents []stdlib.SubAgentRef, hasSubAgents bool) []loom.StepInfo {
	// chat 的后继：编排形态指向逻辑节点 "router"，单 agent 形态为空（END）。
	chatNext := ""
	if hasSubAgents {
		chatNext = "router"
	}
	// 两种拓扑共有的骨架：prompt_assemble → chat；chat 节点标注所用模型。
	topo := []loom.StepInfo{
		{Name: "prompt_assemble", Edges: []loom.Edge{{To: "chat"}}},
		{Name: "chat", Detail: model, Edges: []loom.Edge{{To: chatNext}}},
	}
	if hasSubAgents {
		// 编排形态：补上 router 节点及其到各子 agent 的分支边。
		var branches []loom.Edge
		for _, sa := range subAgents {
			key := sa.RouteKey
			// 与 buildSubAgentRouter 保持一致：RouteKey 缺省为 Name，作为边的标签展示。
			if key == "" {
				key = sa.Name
			}
			branches = append(branches, loom.Edge{To: "sub_" + sa.Name, Label: key})
		}
		// 追加"无委派"分支：路由查不到时直接 END，与运行时回退语义对应。
		branches = append(branches, loom.Edge{To: "", Label: "no delegation"})
		topo = append(topo, loom.StepInfo{Name: "router", Edges: branches})
		// 每个子 agent 节点：Detail 标注子 agent 名，出边为 END（执行完即整图结束）。
		for _, sa := range subAgents {
			topo = append(topo, loom.StepInfo{Name: "sub_" + sa.Name, Detail: sa.Name, Edges: []loom.Edge{{To: ""}}})
		}
	}
	return topo // 返回完整拓扑声明
}
