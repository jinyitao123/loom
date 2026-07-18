// run.go —— `loom run` 子命令：一轮 agent 的完整装配线。
// 从 stdin 读入 prompt JSON → 装配 LLM/工具/会话存储 → 按 spec 选择单体路径或
// 编译编排图并执行 → 落盘会话 → 向 stdout 输出 NDJSON 事件流
// （即 weave 守护进程 spawn-harness 所消费的机器协议）。

package main

import (
	"bytes"         // stdin 原始字节的裁剪与 '{' 前缀判断
	"context"       // 贯穿一轮 agent 执行的取消/超时上下文
	"encoding/json" // prompt JSON 解析与工具入参的原样透传
	"flag"          // `loom run` 子命令的旗标解析
	"fmt"           // 错误包装与 text 格式的人类可读输出
	"io"            // stdin 全量读取与输出目标抽象
	"os"            // 真实进程的 stdio/env/chdir 接入
	"path/filepath" // 在 agent cwd 下拼接配置文件路径
	"strings"       // 模型 id 前缀匹配与系统 prompt 片段拼接
	"time"          // MCP 预检的超时上限

	"github.com/google/uuid"                        // 新会话 id 的随机生成
	"github.com/jinyitao123/loom"                   // 核心库：状态/存储/图/步骤等编排原语
	"github.com/jinyitao123/loom/contract"          // LLM/工具/消息等稳定契约类型
	"github.com/jinyitao123/loom/provider/deepseek" // DeepSeek LLM 提供方
	"github.com/jinyitao123/loom/provider/openai"   // OpenAI（及一切兼容端点）LLM 提供方
	"github.com/jinyitao123/loom/stdlib"            // 标准步骤库：prompt 装配、工具循环、会话读写
	"github.com/jinyitao123/loom/stdlib/compiler"   // AgentSpec → 确定性路由图的编译器
)

// sink is the event output surface. *Emitter (stream-json) and textEmitter
// (human-readable) both satisfy it.
// sink 是事件输出面：一轮 agent 的所有可观测事件都经由它落到 stdout。
// *Emitter 产出 NDJSON（weave 守护进程消费的机器协议），textEmitter 产出人类可读文本；
// 两者实现同一接口，使 runAgent 与具体输出格式完全解耦。
type sink interface {
	SessionInit(id string) error                          // 宣告本轮会话 id（新建或恢复），协议要求它是首个事件
	Text(text string) error                               // 模型可见回复的流式文本增量
	Thinking(text string) error                           // 模型思考（reasoning）文本增量
	ToolUse(id, name string, input json.RawMessage) error // 一次工具调用的发起；id 用于与结果配对
	ToolResult(id, output, status string) error           // 工具调用的结果回填，status 为 success/error
	TurnEnd(sessionID string) error                       // 一轮对话结束的边界标记
	Error(message string) error                           // 错误上报通道
	Result(status, output, sessionID string) error        // 终态事件：completed/failed + 最终输出，必须是最后一个事件
}

// runConfig holds the parsed `loom run` flags.
// runConfig 承载 `loom run` 全部命令行旗标的解析结果，作为一轮执行的静态配置向下传递。
type runConfig struct {
	format      string // 输出格式：stream-json（NDJSON 机器协议）或 text（人类可读）
	cwd         string // agent 的工作目录：配置文件解析基准 + 执行前 chdir 目标
	model       string // 模型 id，同时决定提供方选择（如 deepseek-* → DeepSeek）
	resume      string // 非空则恢复既有会话：以此 id 加载历史消息并续写
	bypass      bool   // 非交互模式下自动放行所有工具调用（权限旁路开关）
	mcpConfig   string // .loom-mcp.json 的显式路径，空则回退到 <cwd>/.loom-mcp.json
	agentConfig string // agent spec JSON 的显式路径，空则回退到 <cwd>/.loom-agent.json
	profile     string // 激活的 profile 名，参与系统 prompt 装配
}

// resolveCwdFile picks a config file: an explicit flag wins, else <name> under
// the agent cwd (bare <name> when no cwd).
// resolveCwdFile 统一配置文件的定位规则：显式旗标 > agent cwd 下的约定文件名 >
// 当前目录裸文件名 —— 三个来源的优先级在此单点固化。
func resolveCwdFile(flag, cwd, name string) string {
	if flag != "" { // 显式旗标最高优先：调用方点名的路径原样返回
		return flag
	}
	if cwd != "" { // 有 agent cwd 时，配置文件约定随 agent 工作目录走
		return filepath.Join(cwd, name)
	}
	return name // 无 cwd（本地手测场景）：退化为相对当前目录的裸文件名
}

// resolveMCPPath picks the MCP config file (.loom-mcp.json by default).
// resolveMCPPath 是 resolveCwdFile 针对 MCP 配置文件名的便捷封装。
func resolveMCPPath(flag, cwd string) string { return resolveCwdFile(flag, cwd, ".loom-mcp.json") }

// promptInput is the JSON the weave daemon writes to stdin. Only the fields we
// consume in v1 are modeled; unknown fields are ignored.
// promptInput 建模 weave 守护进程写入 stdin 的 prompt JSON。
// 只声明 v1 实际消费的字段，未知字段由 json.Unmarshal 静默忽略 —— 协议前向兼容。
type promptInput struct {
	Type        string `json:"type"`        // 消息类型（v1 未按其分派，仅保留协议位）
	Instruction string `json:"instruction"` // 本轮任务指令，即用户消息正文（JSON 形态下必填）
	Notice      string `json:"notice"`      // 守护进程附带的临时告示，将被折入系统 prompt
	// RecalledMemory is semantic memory the weave daemon recalled for this task
	// (memory recall/remember runs daemon-side, around the spawn, for every
	// provider). loom just folds it into the system prompt.
	// RecalledMemory 是守护进程在 spawn 前后统一完成的语义记忆召回结果；
	// loom 不负责召回与写回，只把它折叠进系统 prompt 供模型参考。
	RecalledMemory []string `json:"recalled_memory"`
}

// parsePrompt reads the stdin payload. A JSON object must carry "instruction";
// anything else is treated as a plain-text instruction (human-testing
// convenience).
// parsePrompt 解析 stdin 载荷，兼容两种形态：JSON 对象走完整协议（必须含 instruction），
// 其余任意文本直接当作指令 —— 方便人工 `echo "..." | loom run` 手测。
func parsePrompt(raw []byte) (promptInput, error) {
	var p promptInput
	trimmed := bytes.TrimSpace(raw) // 去掉首尾空白，让空输入检测与 '{' 前缀判断都基于有效内容
	if len(trimmed) == 0 {
		return p, fmt.Errorf("empty prompt on stdin") // 空输入是硬错误：没有任务就没有本轮执行
	}
	if trimmed[0] == '{' { // 以 '{' 开头即按 JSON 协议解析（简单但足够的形态判别）
		if err := json.Unmarshal(trimmed, &p); err != nil {
			// JSON 形态但解析失败：拒绝而非降级为纯文本，避免把协议错误吞成奇怪的指令
			return p, fmt.Errorf("invalid prompt JSON: %w", err)
		}
		if p.Instruction == "" {
			return p, fmt.Errorf("prompt JSON has no 'instruction' field") // instruction 是协议必填项，缺失即拒绝
		}
		return p, nil
	}
	p.Instruction = string(trimmed) // 非 JSON：整段文本即指令，其余字段保持零值
	return p, nil
}

// firstNonEmpty 返回首个非空字符串，用于表达环境变量取值的优先级链。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals { // 传入顺序即优先级顺序，逐一检查
		if v != "" {
			return v
		}
	}
	return "" // 全部为空则返回空串，是否视为错误由调用方决定
}

// buildLLM selects a provider from the model id + environment. A LOOM_BASE_URL
// override targets any OpenAI-compatible endpoint (Ollama, vLLM, OpenRouter…).
// buildLLM 依据模型 id 与环境变量装配 LLM 客户端；getenv 以参数注入，便于测试替换环境。
// 选择顺序：LOOM_BASE_URL 覆盖一切 → 模型前缀路由到专有提供方 → 默认 OpenAI。
func buildLLM(model string, getenv func(string) string) (contract.LLM, error) {
	if base := getenv("LOOM_BASE_URL"); base != "" { // 显式端点覆盖：任何 OpenAI 兼容服务（Ollama、vLLM、OpenRouter 等）
		key := firstNonEmpty(getenv("LOOM_API_KEY"), getenv("OPENAI_API_KEY"))            // 密钥优先取 LOOM_API_KEY；允许为空（本地端点常不鉴权）
		opts := []openai.Option{openai.WithBaseURL(base), openai.WithDefaultModel(model)} // 指向自定义端点并固化默认模型
		if getenv("LOOM_JSON_OBJECT_MODE") != "" {                                        // 部分兼容端点只支持 json_object 不支持 json_schema：由环境开关降级
			opts = append(opts, openai.WithJSONObjectMode())
		}
		return openai.New(key, opts...), nil
	}
	switch {
	case strings.HasPrefix(model, "deepseek"): // 模型 id 前缀路由：deepseek-* 走 DeepSeek 官方端点
		key := firstNonEmpty(getenv("DEEPSEEK_API_KEY"), getenv("LOOM_API_KEY")) // 专有密钥优先，通用 LOOM_API_KEY 兜底
		if key == "" {
			// 云端点无密钥无法工作：装配期即失败，错误里带上模型名便于定位配置来源
			return nil, fmt.Errorf("DEEPSEEK_API_KEY not set (model %q)", model)
		}
		return deepseek.New(key), nil
	default: // 其余模型一律按 OpenAI 处理（gpt-*、o* 以及未知 id）
		key := firstNonEmpty(getenv("OPENAI_API_KEY"), getenv("LOOM_API_KEY"))
		if key == "" {
			return nil, fmt.Errorf("OPENAI_API_KEY not set (model %q)", model) // 同上：缺密钥立即失败
		}
		return openai.New(key, openai.WithDefaultModel(model)), nil
	}
}

// noTools is the v1 empty tool dispatcher. MCP wiring (reading .loom-mcp.json
// in cwd → an mcphost HTTP dispatcher) is a follow-up (weave C.7).
// noTools 是"零工具"占位分发器：无 MCP 配置时 agent 依然可以纯对话运行。
type noTools struct{}

// ListTools 返回空清单：模型侧看不到任何工具，自然不会发起调用。
func (noTools) ListTools(context.Context) ([]contract.ToolDef, error) { return nil, nil }

// Dispatch 理论上不可达（清单为空）；真被调用时显式报错，避免静默吞掉异常路径。
func (noTools) Dispatch(context.Context, contract.ToolCall) (*contract.ToolResult, error) {
	return nil, fmt.Errorf("no tools configured (MCP wiring is a follow-up)")
}

// toolEventHook turns loom's per-tool Pre/Post hooks into tool_use / tool_result
// events on the sink. With noTools this is dormant; it activates once MCP tools
// are wired in.
// toolEventHook 把 loom 工具循环的 Pre/Post 钩子桥接为 sink 上的 tool_use / tool_result
// 事件，使外部（weave 守护进程）能实时观测每次工具调用；钩子只旁路观测、不改变调用本身。
func toolEventHook(out sink) contract.ToolHook {
	return contract.ToolHook{
		// Pre 钩子：工具真正执行前发出 tool_use 事件。
		Pre: func(_ context.Context, call contract.ToolCall) (contract.ToolCall, error) {
			var input json.RawMessage
			if call.Args != "" { // 入参非空才透传：空串不是合法 JSON，直接嵌入会破坏 NDJSON 事件
				input = json.RawMessage(call.Args)
			}
			_ = out.ToolUse(call.ID, call.Name, input) // 事件发射失败不阻断工具执行：观测通道的错误不能影响主流程
			return call, nil                           // 原样返回 call：本钩子不修改调用
		},
		// Post 钩子：工具返回后发出与之配对的 tool_result 事件。
		Post: func(_ context.Context, _ contract.ToolCall, result *contract.ToolResult) error {
			status := "success"
			if result.IsError { // 把布尔错误位翻译为协议上的状态字符串
				status = "error"
			}
			_ = out.ToolResult(result.CallID, result.Content, status) // 同样忽略发射错误，保证工具结果照常回填给模型
			return nil
		},
	}
}

// runDeps bundles the runtime collaborators for an agent turn so the signature
// stays small as features (tools, session store, rich config) are added.
// runDeps 打包一轮 agent 执行所需的全部运行时协作者，避免功能扩张时函数签名膨胀。
type runDeps struct {
	llm   contract.LLM            // 底层 LLM 客户端（后续会包一层流式装饰器）
	tools contract.ToolDispatcher // 工具分发器：MCP 聚合器或 noTools 占位
	// store 为会话存储；nil 表示本轮不做会话持久化（既不加载也不落盘）。
	store loom.Store // nil = no session persistence
	// spec 为富配置（身份/技能/profiles/子 agent 声明）；nil 表示裸跑，跳过 prompt 装配与编排。
	spec *stdlib.AgentSpec // nil = no rich config (identity/skills/profiles)
	out  sink              // 事件输出面：本轮所有可观测事件的唯一出口
}

// runAgent drives one agent turn:
//
//	session_init → (load prior session) → assemble system prompt →
//	streaming tool-loop (text deltas + tool events) → (save session) → result
//
// loom.Graph.Run is synchronous; text is streamed by wrapping the LLM
// (streamingLLM). Tool calls surface via ToolHooks. Session continuity (--resume)
// uses the injected store.
// runAgent 是一轮 agent 的总装配线：确定会话 id → 恢复历史 → 组装初始状态 →
// 按 spec 选择单体或编排路径执行 → 成功则落盘会话 → 发出终态事件。
// 图执行本身是同步的，流式文本靠 streamingLLM 装饰器在 LLM 层旁路发射。
func runAgent(ctx context.Context, d runDeps, p promptInput, cfg runConfig) error {
	sessionID := cfg.resume // --resume 提供的 id 直接复用，实现会话延续
	if sessionID == "" {
		sessionID = "lm_" + uuid.NewString() // 新会话：lm_ 前缀 + 随机 uuid，全局唯一且可辨识来源
	}
	_ = d.out.SessionInit(sessionID) // 协议要求首个事件即宣告会话 id，调用方据此关联后续事件

	// Conversation continuity: load prior turns when resuming.
	// 会话连续性：仅在"有存储 且 显式 --resume"时加载历史消息；加载失败静默降级为全新对话。
	var priorMsgs []contract.Message
	if d.store != nil && cfg.resume != "" {
		priorMsgs, _ = stdlib.LoadSession(d.store, sessionID)
	}
	// 历史消息之后追加本轮用户指令，构成送入模型的完整对话上下文。
	messages := append(priorMsgs, contract.Message{Role: "user", Content: p.Instruction})
	// 初始状态：messages 是工具循环消费的协议键；last_user_message 供 prompt 装配/技能匹配使用。
	state := loom.State{"messages": messages, "last_user_message": p.Instruction}

	// 用流式装饰器包住 LLM：文本/思考增量在生成途中即经 sink 外发。
	streaming := &streamingLLM{inner: d.llm, out: d.out}

	// Orchestrating specs (declared sub_agents) run a compiled deterministic-
	// routing graph; everything else keeps the original single tool-loop path
	// byte-for-byte (zero regression).
	// 路径二选一：spec 声明了子 agent → 编排图（确定性路由）；否则走原始单工具循环。
	// 后者保持与旧版本逐字节一致的行为，确保零回归。
	var output string
	var rerr error
	if orchestrating(d.spec) {
		output, rerr = runOrchestrated(ctx, d, p, cfg, streaming, state)
	} else {
		output, rerr = runSingle(ctx, d, p, cfg, streaming, state)
	}
	if rerr != nil {
		_ = d.out.Error(rerr.Error())                 // 先上报错误详情
		_ = d.out.Result("failed", output, sessionID) // 再发 failed 终态；output 可能携带失败前的部分产出
		return rerr
	}

	if d.store != nil { // 仅成功路径落盘会话：追加 assistant 最终回复后整体保存
		finalMsgs := append(messages, contract.Message{Role: "assistant", Content: output})
		_ = stdlib.SaveSession(d.store, sessionID, finalMsgs) // 持久化失败不影响本轮结果，静默容忍
	}
	_ = d.out.Result("completed", output, sessionID) // 协议要求最后一个事件是终态 result
	return nil
}

// orchestrating reports whether the spec declares deterministic sub-agent routing.
// orchestrating 判断是否进入编排路径：唯一依据是 spec 显式声明了子 agent 列表。
func orchestrating(spec *stdlib.AgentSpec) bool {
	return spec != nil && len(spec.SubAgents) > 0
}

// runSingle is the original single-agent path: inline prompt assembly + recalled
// memory/notice fold + one tool loop. Kept byte-identical to the pre-O3 behavior.
// runSingle 是原始单 agent 路径：内联做一次 prompt 装配（身份/技能/profile），
// 再折入召回记忆与告示，最后跑一轮工具循环取输出；行为与编排功能引入前完全一致。
func runSingle(ctx context.Context, d runDeps, p promptInput, cfg runConfig, streaming contract.LLM, state loom.State) (string, error) {
	if d.spec != nil { // 有富配置才装配系统 prompt；裸跑时模型只见用户指令
		// 构造 prompt 装配步骤：身份 + 技能清单 + profile 叠加；
		// KeywordMatcher 按关键词命中决定注入哪些技能正文（技能注入的时机即在此）。
		assemble := stdlib.NewPromptAssembleStep(stdlib.PromptConfig{
			Identity:     specIdentity(d.spec),
			Skills:       d.spec.Skills,
			Profile:      cfg.profile,
			ProfilesMap:  d.spec.Profiles,
			SkillMatcher: &stdlib.KeywordMatcher{},
		})
		// 装配失败静默跳过（系统 prompt 缺失不致命）；成功则把状态增量合并回主状态（含 __system_prompt）。
		if delta, aerr := assemble(ctx, state); aerr == nil {
			for k, v := range delta {
				state[k] = v
			}
		}
	}
	// 召回记忆与告示追加到已装配的系统 prompt 之后（身份在前，附加信息在后）。
	foldMemoryNotice(state, p)
	// 构造核心工具循环步骤：流式 LLM + 工具分发器 + 事件钩子（tool_use/tool_result 外发）。
	step := stdlib.NewToolLoopStep(streaming, d.tools, stdlib.ToolLoopOpts{
		Model:     cfg.model,
		ToolHooks: []contract.ToolHook{toolEventHook(d.out)},
	})
	result, err := step(ctx, state) // 同步执行直至模型给出最终回复（中途可能有多轮工具调用）
	if err != nil {
		return "", err
	}
	return stdlib.GetString(result, "output", ""), nil // 按协议键 "output" 取最终文本，缺省为空串
}

// runOrchestrated compiles the spec into a deterministic-routing graph and runs
// it. The graph owns prompt assembly, so recalled_memory + notice ride in via
// CompileOpts.Context (not an inline fold). A model `delegate` tool call is
// captured into __delegate_to so the router branches to the chosen sub-agent.
// runOrchestrated 是编排路径：把 AgentSpec 编译成"chat → 确定性路由器 → sub_<name> 子图"
// 的图后整体执行。prompt 装配由图内步骤负责，故记忆/告示改经 CompileOpts.Context 注入；
// 模型的 delegate 调用被捕获进 __delegate_to，路由器据此查表分支。
func runOrchestrated(ctx context.Context, d runDeps, p promptInput, cfg runConfig, streaming contract.LLM, state loom.State) (string, error) {
	sig := &delegateSignal{} // 委派信号：工具结果写不进图状态，靠这枚共享结构把模型的选择带给路由器
	// 在真实工具分发器外再包一层：附加合成的 delegate 工具，捕获委派并透传其余工具。
	tools := newDelegateDispatcher(d.tools, d.spec.SubAgents, sig)
	// 把 spec 编译为可执行的图；各选项分别接管模型、profile、上下文注入、
	// 工具事件钩子、chat 步骤工厂（捕获委派信号）与子 agent 的解析构建。
	g, err := compiler.CompileAgent(d.spec, streaming, tools, compiler.CompileOpts{
		Name:             "agent",
		Model:            cfg.model,
		Profile:          cfg.profile,
		Context:          memoryNoticeContext(p),
		ToolHooks:        []contract.ToolHook{toolEventHook(d.out)},
		ChatStepFactory:  delegateChatStepFactory(sig),
		SubAgentResolver: cliSubAgentResolver(cfg, streaming, d.tools, d.out),
	})
	if err != nil {
		return "", err // 编译期错误（spec 不合法等）直接失败，不产生任何模型调用
	}
	// nil store → no checkpoint files in .loom-sessions; the session transcript
	// is persisted separately by the caller under ns="session".
	// 图执行传入 nil 存储：不生成检查点文件；会话转写由上层 runAgent 单独负责持久化。
	result, err := g.Run(ctx, state, nil)
	if err != nil {
		if result != nil { // 失败但留有终态：尽力提取部分输出随错误一并返回，供 failed 事件携带
			return stdlib.GetString(result.State, "output", ""), err
		}
		return "", err
	}
	return stdlib.GetString(result.State, "output", ""), nil // 成功：从最终状态按协议键 "output" 取回答
}

// foldMemoryNotice appends recalled memory + notice AFTER the assembled
// identity/skills block (identity first), preserving the pre-O3 ordering.
// foldMemoryNotice 把召回记忆与告示折入 __system_prompt：始终排在身份/技能块之后，
// 维持"身份优先"的既有顺序约定（单体路径专用；编排路径改走 Context 注入）。
func foldMemoryNotice(state loom.State, p promptInput) {
	extras := memoryNoticeLines(p) // 先物化待追加的片段
	if len(extras) == 0 {
		return // 无附加信息：不触碰状态，避免写入空的系统 prompt
	}
	parts := extras
	if sp, _ := state["__system_prompt"].(string); sp != "" { // 已有装配好的系统 prompt：置于最前
		parts = append([]string{sp}, extras...)
	}
	state["__system_prompt"] = strings.Join(parts, "\n\n") // 空行分隔各片段，回写协议键 __system_prompt
}

// memoryNoticeLines 把 prompt 里的召回记忆与告示物化为系统 prompt 片段列表。
func memoryNoticeLines(p promptInput) []string {
	var extras []string
	if len(p.RecalledMemory) > 0 { // 记忆片段合并成一个带标题的 Markdown 小节，逐条换行
		extras = append(extras, "## Relevant memory from past tasks\n"+strings.Join(p.RecalledMemory, "\n"))
	}
	if p.Notice != "" { // 告示原样作为独立片段追加
		extras = append(extras, p.Notice)
	}
	return extras
}

// memoryNoticeContext surfaces recalled memory + notice to the compiled graph's
// prompt_assemble (rendered under "## 当前上下文") so the orchestrating path gets
// them without a second inline assembly.
// memoryNoticeContext 把召回记忆与告示转成编译图 prompt 装配步骤可消费的上下文 map，
// 使编排路径无需第二次内联装配即可获得与单体路径同等的信息。
func memoryNoticeContext(p promptInput) map[string]any {
	if len(p.RecalledMemory) == 0 && p.Notice == "" {
		return nil // 两者皆空：返回 nil 让编译器完全跳过上下文小节
	}
	c := map[string]any{}
	if len(p.RecalledMemory) > 0 {
		c["recalled_memory"] = strings.Join(p.RecalledMemory, "\n") // 多条记忆合并为一个换行分隔的字符串值
	}
	if p.Notice != "" {
		c["notice"] = p.Notice // 告示作为独立键传入
	}
	return c
}

// textEmitter is the human-readable sink for `--format text`.
// textEmitter 是 --format text 的人类可读 sink：只保留对肉眼有意义的事件，
// 会话/回合边界与思考流一律静默丢弃。
type textEmitter struct{ w io.Writer }

// SessionInit 会话 id 对人类读者无意义，直接丢弃。
func (t textEmitter) SessionInit(string) error { return nil }

// Text 把文本增量原样写出，不加装饰，拼起来即模型回复。
func (t textEmitter) Text(s string) error { _, err := fmt.Fprint(t.w, s); return err }

// Thinking 思考流在 text 模式下不展示。
func (t textEmitter) Thinking(string) error { return nil }

// ToolUse 以 "→ 工具名 入参" 单行标注一次工具调用的发起。
func (t textEmitter) ToolUse(_, name string, input json.RawMessage) error {
	_, err := fmt.Fprintf(t.w, "\n→ %s %s\n", name, string(input))
	return err
}

// ToolResult 以 "← 输出" 单行标注工具结果，与 → 行成对呼应。
func (t textEmitter) ToolResult(_, output, _ string) error {
	_, err := fmt.Fprintf(t.w, "← %s\n", output)
	return err
}

// TurnEnd 回合边界对人类读者无意义，丢弃。
func (t textEmitter) TurnEnd(string) error { return nil }

// Error 以醒目的 ERROR: 前缀独立成行输出错误。
func (t textEmitter) Error(m string) error {
	_, err := fmt.Fprintf(t.w, "\nERROR: %s\n", m)
	return err
}

// Result 只打印终态（[completed]/[failed]）；输出正文此前已流式打印过，不再重复。
func (t textEmitter) Result(status, _, _ string) error {
	_, err := fmt.Fprintf(t.w, "\n[%s]\n", status)
	return err
}

// newSink 按 --format 选择输出面：text → 人类可读；其余（默认 stream-json）→ NDJSON 事件流。
func newSink(format string, w io.Writer) sink {
	if format == "text" {
		return textEmitter{w: w}
	}
	return NewEmitter(w) // 默认机器协议：weave 守护进程逐行解析的 NDJSON
}

// runCmd implements `loom run`, wired to the process's real stdio/env.
// runCmd 是生产入口：把真实进程的 stdin/stdout/环境接进可测试核心 runCmdIO。
func runCmd(args []string) int {
	return runCmdIO(args, os.Stdin, os.Stdout, os.Getenv)
}

// runCmdIO is the testable core of `loom run` — stdio and env are injected so it
// can be driven in-process (the subprocess E2E tests cover the real binary; this
// covers the orchestration logic).
// runCmdIO 是 `loom run` 的可测核心：stdio 与 env 全部注入，测试可在进程内驱动全流程。
// 返回值即进程退出码：0 成功、1 运行失败、2 参数用法错误。
func runCmdIO(args []string, stdin io.Reader, stdout io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError) // 独立 FlagSet：解析失败返回错误而非直接退出进程
	fs.SetOutput(io.Discard)                           // 抑制 flag 包自带的 usage 打印，错误统一走事件协议
	// 逐一声明 `loom run` 支持的旗标（含默认值与帮助文案）。
	format := fs.String("format", "stream-json", "output format: stream-json | text")
	cwd := fs.String("cwd", "", "working directory for the agent")
	model := fs.String("model", "", "model id (e.g. deepseek-chat, gpt-4o)")
	resume := fs.String("resume", "", "resume an existing session id")
	bypass := fs.Bool("bypass-permissions", false, "auto-allow all tool calls (non-interactive)")
	mcpConfig := fs.String("mcp-config", "", "path to .loom-mcp.json (default: <cwd>/.loom-mcp.json)")
	agentConfig := fs.String("agent-config", "", "path to the agent spec JSON (default: <cwd>/.loom-agent.json)")
	profile := fs.String("profile", "", "active profile name")
	if err := fs.Parse(args); err != nil {
		return 2 // 参数解析失败：约定退出码 2（用法错误）；此时 sink 尚未建立，不发事件
	}
	// 把解析结果收敛为一个配置值向下传递。
	cfg := runConfig{format: *format, cwd: *cwd, model: *model, resume: *resume, bypass: *bypass, mcpConfig: *mcpConfig, agentConfig: *agentConfig, profile: *profile}

	out := newSink(cfg.format, stdout) // sink 就绪之后，一切错误都要以事件形式出现在输出流里

	// fail 统一失败收尾：error 事件 + failed 终态 + 退出码 1，保证协议完整性。
	fail := func(msg string) int {
		_ = out.Error(msg)
		_ = out.Result("failed", "", cfg.resume)
		return 1
	}

	raw, err := io.ReadAll(stdin) // 一次性读完 stdin（协议是单个 prompt 载荷，非流式输入）
	if err != nil {
		return fail(fmt.Sprintf("read stdin: %v", err))
	}
	p, err := parsePrompt(raw) // 解析 prompt：JSON 协议或纯文本兜底
	if err != nil {
		return fail(err.Error())
	}
	llm, err := buildLLM(cfg.model, getenv) // 依据模型 id + 环境装配 LLM；缺密钥在此即失败
	if err != nil {
		return fail(err.Error())
	}
	// 加载 MCP 工具分发器；配置文件缺失会得到 noTools 而非错误。
	tools, err := loadMCPDispatcher(resolveMCPPath(cfg.mcpConfig, cfg.cwd))
	if err != nil {
		return fail(err.Error())
	}
	// MCP 预检：引擎启动前先完成 initialize 握手 + 工具清单聚合，必需服务器失败在此
	// 大声报错（fail loudly），避免带着残缺工具集进入模型调用。
	// 用接口断言探测预检能力：noTools 没有 Preflight，天然跳过。
	if preflight, ok := tools.(interface{ Preflight(context.Context) error }); ok {
		preflightCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second) // 预检上限 60 秒，防止挂死在无响应的服务器上
		err := preflight.Preflight(preflightCtx)
		cancel() // 预检结束立即释放定时器资源（不用 defer：函数后续还有长生命周期逻辑）
		if err != nil {
			return fail("mcp preflight: " + err.Error())
		}
	}
	// 加载 agent spec（身份/技能/子 agent 等富配置）；文件缺失同样非致命。
	spec, err := loadAgentSpec(resolveCwdFile(cfg.agentConfig, cfg.cwd, ".loom-agent.json"))
	if err != nil {
		return fail(err.Error())
	}
	// 会话目录固定为 <cwd>/.loom-sessions；必须在 chdir 之前解析，保证路径基准正确。
	sessionDir := resolveCwdFile("", cfg.cwd, ".loom-sessions")
	if cfg.cwd != "" { // 切换进程工作目录：让工具调用产生的相对路径都落在 agent cwd 下
		if err := os.Chdir(cfg.cwd); err != nil {
			return fail(fmt.Sprintf("chdir %s: %v", cfg.cwd, err))
		}
	}
	// 汇齐全部运行时协作者，进入一轮 agent 执行。
	deps := runDeps{llm: llm, tools: tools, store: newFileStore(sessionDir), spec: spec, out: out}
	if err := runAgent(context.Background(), deps, p, cfg); err != nil {
		return 1 // 错误事件已在 runAgent 内部发出，这里只需传达非零退出码
	}
	return 0
}
