package loom

import (
	"context"       // 取消/超时传播，同时承载跨子图共享的全局预算计数器
	"encoding/json" // 检查点的序列化格式
	"fmt"           // 拼装带上下文的错误信息
	"log/slog"      // 尽力而为检查点失败时的结构化告警日志
	"sync/atomic"   // 全局步数预算的并发安全计数器
	"time"          // 检查点落盘时间戳

	"github.com/google/uuid" // 生成运行标识 __run_id
)

// Graph is an executable composition of Steps and Routers.
// Graph（图）是步骤与路由器的可执行组合体，是内核对外的执行入口。
// 一切上层能力（agent、子图、HITL）都由这一个结构承载；字段在构造期定型，运行期只读，
// 因此同一个 Graph 可被多个 goroutine 并发 Run（各自的状态互相独立）。
type Graph struct {
	Name  string          // 图名：用作检查点命名空间（"checkpoint:"+Name）及日志/错误标识
	steps map[string]Step // 步骤表：步骤名 → 步骤实现
	// routers：步骤名 → 该步骤执行后的路由器；查不到即视为终端步骤（执行完停机）。
	routers map[string]Router
	// topology：由图构建器声明的拓扑描述，仅供可视化导出，不参与实际执行。
	topology []StepInfo // declared by graph builder for visualization
	entry    string     // 入口步骤名：Run 由此开始；Resume 通过 withEntry 换入口重入
	hooks    HookPoints // 前置/后置钩子集合
	// mergeConfig：合并配置，挂到图上即被 NewGraph 冻结，保证运行期合并语义稳定。
	mergeConfig      *MergeConfig
	checkpointPolicy CheckpointPolicy // 检查点失败策略：尽力而为（默认）或强制落盘
	maxIter          int              // 单图迭代上限：第一层熔断（默认 100）
	// stepBudget：全局步数预算初值，第二层熔断；nil 表示未启用，仅根图设置，
	// 运行时装入 atomic 计数器并经 context 传给所有子图共享。
	stepBudget *int64
}

// StopReason classifies why a graph execution ended.
// StopReason（停机原因）对图执行的终止方式做枚举分类，供调用方在结果上做程序化分支。
type StopReason string

const (
	// StopCompleted：正常结束——路由器返回空字符串表示停机。
	StopCompleted StopReason = "completed" // normal termination (router returned "")
	// StopYielded：HITL 暂停——步骤置 __yield 主动让出控制权，等待人工介入后 Resume。
	StopYielded StopReason = "yielded" // HITL pause (__yield)
	// StopMaxIter：单图迭代熔断——本图步数超过 maxIter 上限。
	StopMaxIter StopReason = "max_iter" // per-graph circuit breaker
	// StopBudget：全局预算耗尽——父图与所有子图共享的步数预算被扣完。
	StopBudget StopReason = "budget" // global step budget exhausted
	// StopError：步骤返回非 nil error（错误现场已写入 __error / __failed_step）。
	StopError StopReason = "error" // Step returned non-nil error
	// StopHookAbort：前置/后置钩子返回 error，行使否决权中止执行。
	StopHookAbort StopReason = "hook_abort" // Before/After hook returned error
)

// RunResult contains the final state and metadata of a graph execution.
// RunResult 是一次图执行的结果：停机时的完整状态 + 停机元数据。
type RunResult struct {
	State      State      // 停机时的完整状态快照（可能含 __error 等引擎协议键）
	LastStep   string     // 最后执行（或试图执行）的步骤名，Resume 与排障都依赖它定位
	Yielded    bool       // 是否因 HITL 暂停而停：true 时调用方应保留 RunID 以便后续 Resume
	Steps      int        // 本次调用实际执行的步骤数（不含 Resume 之前的历史步数）
	RunID      string     // 运行标识（__run_id）：同一次业务运行跨 Run/Resume 保持不变，是检查点的主键
	StopReason StopReason // 停机原因分类
}

// checkpoint 是落盘到 Store 的检查点结构：一步执行并合并之后的自洽快照。
// 存储位置：命名空间 "checkpoint:"+Graph、键为 RunID——同一运行只保留最新一份，覆盖写。
type checkpoint struct {
	RunID    string `json:"run_id"`    // 运行标识（__run_id），检查点主键
	Graph    string `json:"graph"`     // 图名，冗余记录便于排障
	LastStep string `json:"last_step"` // 快照对应的步骤名：Resume 由此决定从哪一步重入
	State    State  `json:"state"`     // 合并后的完整状态快照（自洽，可独立恢复现场）
	// YieldPhase：暂停阶段协议——"mid_step"=恢复时重跑该步骤；"after_step"=恢复时跳过该步骤直接走路由；
	// 空串兼容 v1.0 旧检查点（按 mid_step 处理）。
	YieldPhase string    `json:"yield_phase"`
	SavedAt    time.Time `json:"saved_at"` // 落盘时间戳，便于审计与过期清理
}

// budgetKey 是 context 中存放全局步数预算的私有键类型：
// 用未导出的空结构体类型避免与其他包的 context 键冲突；
// 预算之所以走 context 而不是 Graph 字段，是为了让子图（乃至 withEntry 拷贝）天然继承同一个计数器。
type budgetKey struct{}

// NewGraph creates a new graph with the given entry step.
// NewGraph 构造一个图：设定图名与入口步骤，应用函数式选项，最后冻结合并配置。
func NewGraph(name string, entry string, opts ...GraphOption) *Graph {
	g := &Graph{
		Name:             name,
		steps:            make(map[string]Step),   // 预建空步骤表，AddStep 直接写入
		routers:          make(map[string]Router), // 预建空路由表
		entry:            entry,                   // Run 的起点
		maxIter:          100,                     // 单图熔断默认上限，可被 WithMaxIterations 覆盖
		checkpointPolicy: CheckpointBestEffort,    // 默认尽力而为落检查点，可被 WithCheckpointPolicy 覆盖
	}
	// 依次应用构造期选项：选项只在此刻生效，图建成后配置即定型。
	for _, opt := range opts {
		opt(g)
	}
	// 挂载即冻结合并配置：此后 Register 会 panic——
	// 防止运行期改策略导致同一图内不同时刻合并语义不一致，破坏检查点可重放性。
	if g.mergeConfig != nil {
		g.mergeConfig.frozen = true
	}
	return g
}

// AddStep registers a step with an optional router for the "after" transition.
// AddStep 注册一个步骤及其“执行后”路由器；after 传 nil 表示该步骤是终端步骤，执行完即整图停机。
func (g *Graph) AddStep(name string, step Step, after Router) {
	g.steps[name] = step // 同名重复注册以最后一次为准
	// 仅在提供了路由器时登记：Run 主循环里查不到路由器即视为终点。
	if after != nil {
		g.routers[name] = after
	}
}

// StepInfo describes a step and its outgoing edges for topology export.
// StepInfo 描述一个步骤及其全部出边，仅用于拓扑导出/可视化。
type StepInfo struct {
	Name string `json:"name"` // 步骤名
	// Detail：人类可读的补充说明（如该步骤使用的模型或工具）。
	Detail string `json:"detail,omitempty"` // human-readable annotation
	Edges  []Edge `json:"edges"`            // 出边列表：该步骤可能转移到的目标
}

// SetTopology declares the graph's topology for visualization.
// Called by graph builders (CompileAgent, newMirrorGraph, etc.) after constructing the graph.
// SetTopology 声明图的拓扑（仅供可视化）；由图构建器建图完成后调用，对执行路径无任何影响。
func (g *Graph) SetTopology(topo []StepInfo) {
	g.topology = topo // 整体替换已声明的拓扑
}

// Topology returns the declared topology, or nil if not set.
// Topology 返回已声明的拓扑；未声明时返回 nil，调用方需自行判空。
func (g *Graph) Topology() []StepInfo {
	return g.topology
}

// SetHooks attaches before/after hooks.
// SetHooks 挂载前置/后置钩子集合（整体替换而非追加）。
func (g *Graph) SetHooks(h HookPoints) { g.hooks = h }

// resolveRunID safely extracts or generates a run ID.
// resolveRunID 解析或生成运行标识 __run_id：
// 输入状态里已带非空 __run_id（典型如 Resume 场景）则沿用，保证同一运行跨恢复不换 ID、
// 检查点始终覆盖同一个键；否则生成新 UUID 并写回输入状态，让它随状态一路进检查点。
func resolveRunID(input State) string {
	// 类型断言 + 非空校验：__run_id 必须是非空字符串才视为有效。
	if v, ok := input["__run_id"].(string); ok && v != "" {
		return v
	}
	id := uuid.New().String() // 全新运行：生成 UUID 作为检查点主键
	input["__run_id"] = id    // 写回状态，此后每次落盘都随快照携带
	return id
}

// Run executes the graph from the entry step to completion or yield.
// Run 从入口步骤开始执行图，直到正常完成、HITL 暂停或出错。
// 主循环各阶段固定顺序：单图熔断 → 全局预算 → Before 钩子 → 执行步骤 → 合并增量
// → After 钩子 → 检查点 → yield（暂停）检查 → 路由决定下一跳。
func (g *Graph) Run(ctx context.Context, input State, store Store) (*RunResult, error) {
	runID := resolveRunID(input) // 沿用或生成运行标识：整个运行（含后续 Resume）共用同一 ID

	// Initialize global step budget if set on this graph (root only).
	// 仅根图初始化全局预算：把初值装进 atomic 计数器并挂到 context 上；
	// 子图（以及 withEntry 拷贝）经 context 自动继承同一个计数器，
	// 从而构成与 maxIter 并行的第二层熔断——跨子图共享、总量受控。
	if g.stepBudget != nil {
		remaining := atomic.Int64{}
		remaining.Store(*g.stepBudget)                        // 装入预算初值
		ctx = context.WithValue(ctx, budgetKey{}, &remaining) // 存指针：所有子图递减的是同一个计数器
	}

	state := input // 直接以输入为初始状态（后续 Merge 每次都产出新 map，不会回写 input 之外的内容）
	// Clean residual yield keys (e.g. from checkpoint state on Resume).
	// 清除残留的暂停协议键：Resume 时检查点状态里可能仍带上一轮的 __yield / __yield_phase，
	// 不清掉会导致刚恢复就被误判为再次暂停。
	delete(state, "__yield")
	delete(state, "__yield_phase")

	current := g.entry // 当前待执行的步骤名，从入口开始
	steps := 0         // 本次调用已执行步数，供下方 maxIter 熔断检查使用

	// 主循环：current 为空串即停机（与路由器协议一致）。
	for current != "" {
		// Per-graph circuit breaker.
		// 第一层熔断（单图）：先把本次迭代计入 steps，超过 maxIter 立即带错停机，防止路由环路死循环。
		steps++
		if steps > g.maxIter {
			return &RunResult{State: state, LastStep: current, Steps: steps, RunID: runID, StopReason: StopMaxIter},
				fmt.Errorf("loom: graph %q exceeded max iterations (%d)", g.Name, g.maxIter)
		}

		// Global step budget check (shared with sub-graphs).
		// 第二层熔断（全局预算）：从 context 取共享的 atomic 计数器（根图未启用则直接跳过）。
		if budget, ok := ctx.Value(budgetKey{}).(*atomic.Int64); ok {
			// 原子扣减一步：结果为负说明预算已被本图或其他子图用尽，立即带错停机。
			if budget.Add(-1) < 0 {
				return &RunResult{State: state, LastStep: current, Steps: steps, RunID: runID, StopReason: StopBudget},
					fmt.Errorf("loom: global step budget exhausted in graph %q", g.Name)
			}
		}

		// 查步骤表：路由到了未注册的步骤名属于拓扑错误，按 StopError 停机。
		step, ok := g.steps[current]
		if !ok {
			return &RunResult{State: state, LastStep: current, RunID: runID, StopReason: StopError},
				fmt.Errorf("loom: step %q not found in graph %q", current, g.Name)
		}

		// Before hooks.
		// 前置钩子：按注册顺序执行，任一报错即以 StopHookAbort 中止（本步骤不会被执行）。
		for _, hook := range g.hooks.Before {
			if err := hook(ctx, current, state); err != nil {
				return &RunResult{State: state, LastStep: current, RunID: runID, StopReason: StopHookAbort}, err
			}
		}

		// Execute step.
		// 执行步骤：按协议步骤只返回状态增量，合并由引擎在下方统一完成。
		update, err := step(ctx, state)
		if err != nil {
			// 错误路径：先把错误现场（__error=错误文本、__failed_step=出错步骤名）合并进状态，
			// 再尽力落一次检查点——刻意忽略落盘返回值，落盘失败也不能掩盖原始错误；
			// 这样排障与人工修复时能直接从存储里读到完整现场。
			state = state.Merge(State{
				"__error": err.Error(), "__failed_step": current,
			}, g.mergeConfig)
			_ = g.doCheckpoint(ctx, store, runID, current, state, "") // 尽力保存现场；错误检查点不携带 yield 阶段
			return &RunResult{State: state, LastStep: current, RunID: runID, StopReason: StopError}, err
		}
		state = state.Merge(update, g.mergeConfig) // 正常路径：按合并配置把增量并入状态，得到新的自洽快照

		// After hooks.
		// 后置钩子：在增量合并之后执行，能观察到本步骤的完整结果；报错同样以 StopHookAbort 中止。
		for _, hook := range g.hooks.After {
			if err := hook(ctx, current, state); err != nil {
				return &RunResult{State: state, LastStep: current, RunID: runID, StopReason: StopHookAbort}, err
			}
		}

		// Checkpoint.
		// 检查点：每个步骤成功后落盘一次。yield 阶段（__yield_phase）取自状态：
		// 步骤若声明了暂停阶段，它会随快照一起持久化，Resume 靠它决定重入方式。
		// 落盘失败是否中止由 checkpointPolicy 决定（doCheckpoint 内部分流）。
		yieldPhase, _ := state["__yield_phase"].(string)
		if err := g.doCheckpoint(ctx, store, runID, current, state, yieldPhase); err != nil {
			return &RunResult{State: state, LastStep: current, RunID: runID, StopReason: StopError}, err
		}

		// Check yield (HITL pause).
		// 暂停检查（HITL 协议）：步骤置 __yield=true 表示请求人工介入、让出控制权。
		if yielded, _ := state["__yield"].(bool); yielded {
			delete(state, "__yield") // 消费掉暂停标记：它只是一次性控制信号，不应留在返回状态里（阶段信息已随检查点持久化）
			// 以 Yielded=true 正常返回（error 为 nil）：调用方持 RunID 待人工处理后调用 Resume。
			return &RunResult{
				State: state, LastStep: current, Yielded: true,
				Steps: steps, RunID: runID, StopReason: StopYielded,
			}, nil
		}

		// Route to next step.
		// 路由：查当前步骤的路由器；没有登记路由器即视为终端步骤，直接跳出停机。
		router := g.routers[current]
		if router == nil {
			break
		}
		prevStep := current               // 记住刚执行完的步骤：路由报错或停机时 LastStep 应指向它
		current, err = router(ctx, state) // 下一跳由确定性代码（而非模型）根据合并后的状态裁决
		if err != nil {
			return &RunResult{State: state, LastStep: prevStep, RunID: runID, StopReason: StopError}, err
		}
		// 路由器返回空串 = 正常停机：把 current 还原为刚执行的步骤名再跳出，
		// 使返回结果的 LastStep 指向真实的最后一步而不是空串。
		if current == "" {
			current = prevStep
			break
		}
	}

	// 正常完成出口：LastStep 为最后执行的步骤，StopReason 为 completed。
	return &RunResult{State: state, LastStep: current, Steps: steps, RunID: runID, StopReason: StopCompleted}, nil
}

// Resume restarts a yielded graph from its checkpoint.
// Resume 从检查点恢复一次已暂停（HITL）的运行：
// 读检查点 → 把人工提供的 input 增量合并进快照状态 → 按 yield_phase 决定重入方式
// （mid_step=重跑暂停的那一步；after_step=跳过该步直接走它的路由）。
func (g *Graph) Resume(ctx context.Context, runID string, input State, store Store) (*RunResult, error) {
	// 按 runID 读取检查点：命名空间与 Run 落盘时一致（"checkpoint:"+图名）。
	data, err := store.Get(ctx, "checkpoint:"+g.Name, runID)
	if err != nil {
		return nil, fmt.Errorf("loom: checkpoint not found for run %s: %w", runID, err)
	}
	var cp checkpoint
	// 反序列化失败即检查点损坏：直接报错，绝不带着脏数据继续跑。
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("loom: corrupt checkpoint for run %s: %w", runID, err)
	}

	// 恢复现场：以检查点快照为底、人工输入为增量做合并——人工反馈按正常合并语义并入状态。
	state := cp.State.Merge(input, g.mergeConfig)
	state["__run_id"] = runID // 显式回写运行标识：后续 Run 沿用同一 ID，检查点继续覆盖同一键

	// 按暂停阶段协议分流重入方式。
	switch cp.YieldPhase {
	case "after_step":
		// after_step：暂停发生在步骤完成之后——该步骤的成果已合并进快照，恢复时不重跑，直接走它的路由。
		router := g.routers[cp.LastStep]
		if router == nil {
			// 暂停的步骤本身是终端步骤：无路可走，恢复即正常结束。
			return &RunResult{State: state, RunID: runID}, nil
		}
		// 用合并了人工输入的状态做路由决策：人工反馈可以改变后续走向。
		nextStep, err := router(ctx, state)
		if err != nil {
			return &RunResult{State: state, LastStep: cp.LastStep, RunID: runID}, err
		}
		if nextStep == "" {
			// 路由返回空串 = 停机：恢复后直接正常结束。
			return &RunResult{State: state, LastStep: cp.LastStep, RunID: runID}, nil
		}
		// 以路由结果为新入口继续执行：withEntry 浅拷贝共享全部配置，预算经 context 继承。
		return g.withEntry(nextStep).Run(ctx, state, store)

	default: // "mid_step" or empty (v1.0 compat)
		// mid_step（或空串，兼容 v1.0 旧检查点）：暂停发生在步骤中途——恢复时重跑该步骤，
		// 由步骤自身根据合并进来的人工输入决定后续行为，因此声明 mid_step 的步骤必须设计为可重入。
		return g.withEntry(cp.LastStep).Run(ctx, state, store)
	}
}

// doCheckpoint handles persistence with configurable policy.
// doCheckpoint 按配置策略把当前执行现场落成检查点。
// 两种策略的取舍：CheckpointRequired 落盘失败即中止（保可恢复性，宁停不丢恢复点）；
// CheckpointBestEffort（默认）失败只告警继续跑（保可用性，接受崩溃后可能丢最新进度）。
func (g *Graph) doCheckpoint(ctx context.Context, store Store, runID, step string, state State, yieldPhase string) error {
	// 未提供存储 = 调用方明确选择无持久化运行（如纯内存测试），静默跳过。
	if store == nil {
		return nil
	}
	// 组装检查点：完整状态 + 步骤位置 + 暂停阶段 + 时间戳，构成可独立恢复的自洽快照。
	cp := checkpoint{
		RunID:      runID,
		Graph:      g.Name,
		LastStep:   step,
		State:      state,
		YieldPhase: yieldPhase,
		SavedAt:    time.Now(),
	}
	data, err := json.Marshal(cp) // 序列化为 JSON（State 的值必须可 JSON 化）
	if err != nil {
		// 序列化失败属于编程错误（状态里塞了不可 JSON 化的值），无论何种策略都必须上抛暴露。
		return fmt.Errorf("loom: checkpoint marshal failed at step %q: %w (check State for non-JSON values)", step, err)
	}
	// 落盘：同一 runID 覆盖写，存储中始终只保留该运行最新一步的快照。
	err = store.Put(ctx, "checkpoint:"+g.Name, runID, data)
	if err != nil {
		// 写入失败按策略分流。
		switch g.checkpointPolicy {
		case CheckpointRequired:
			// 强制落盘策略：错误上抛，Run 会以 StopError 中止——宁可停机也不能失去恢复点。
			return fmt.Errorf("loom: checkpoint failed at step %q: %w", step, err)
		default:
			// 尽力而为策略（默认）：只记结构化 Warn 日志并吞掉错误，让执行继续。
			slog.Warn("loom: checkpoint failed (best-effort)",
				"graph", g.Name, "step", step, "error", err)
			return nil
		}
	}
	return nil // 落盘成功
}

// withEntry creates a shallow copy of the graph with a different entry point.
// The copy shares steps, routers, hooks, and mergeConfig but does NOT
// re-initialize the step budget (budget is inherited via context).
// withEntry 以不同入口浅拷贝出一个图，供 Resume 从任意步骤重入。
// 关键取舍：stepBudget 置 nil 而非复制——预算早在根图 Run 时装入了 context，
// 拷贝若再带预算，Run 会重新初始化计数器、把已消耗的额度清零，等于绕过全局熔断；
// 置 nil 后拷贝图经 context 继承根图的同一个计数器，预算跨恢复持续生效。
func (g *Graph) withEntry(newEntry string) *Graph {
	return &Graph{
		Name:             g.Name,             // 图名不变：检查点仍写同一命名空间
		steps:            g.steps,            // 共享步骤表（运行期只读，无需深拷贝）
		routers:          g.routers,          // 共享路由表
		entry:            newEntry,           // 唯一的差异：新入口
		hooks:            g.hooks,            // 共享钩子
		mergeConfig:      g.mergeConfig,      // 共享已冻结的合并配置，保证合并语义一致
		checkpointPolicy: g.checkpointPolicy, // 沿用检查点失败策略
		maxIter:          g.maxIter,          // 沿用单图熔断上限（Resume 后步数从 0 重新计）
		stepBudget:       nil,                // 刻意不复制预算：预算经 context 继承，避免重复初始化重置额度
	}
}
