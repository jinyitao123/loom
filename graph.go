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
	historyKeep      int              // 按步历史保留数：0=关闭，-1=全留，正数=保留最近 N 条
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

// CurrentCheckpointSchema is the newest checkpoint format this binary can read.
const CurrentCheckpointSchema = 1

// checkpoint 是落盘到 Store 的检查点结构：一步执行并合并之后的自洽快照。
// latest 位于键 RunID；启用历史后，同一份字节还会追加到 RunID/零填充序号，形成只增不改的存档链。
type checkpoint struct {
	RunID     string `json:"run_id"`               // 运行标识（__run_id），latest 检查点主键
	Graph     string `json:"graph"`                // 图名，冗余记录便于排障
	Seq       int64  `json:"seq"`                  // 步序号：从 1 起，由 State 携带并跨 Resume/fork 连续
	ParentRun string `json:"parent_run,omitempty"` // fork 溯源：源 runID；非 fork 运行为空
	ParentSeq int64  `json:"parent_seq,omitempty"` // fork 溯源：源运行的历史步序号
	LastStep  string `json:"last_step"`            // 快照对应的步骤名：Resume 由此决定从哪一步重入
	State     State  `json:"state"`                // 合并后的完整状态快照（自洽，可独立恢复现场）
	// YieldPhase：暂停阶段协议——"mid_step"=恢复时重跑该步骤；"after_step"=恢复时跳过该步骤直接走路由；
	// 空串兼容 v1.0 旧检查点（按 mid_step 处理）。
	YieldPhase string         `json:"yield_phase"`
	SavedAt    time.Time      `json:"saved_at"`       // 落盘时间戳，便于审计与过期清理
	Schema     int            `json:"schema_version"` // 格式版本；存量无字段检查点反序列化为 0
	Meta       map[string]any `json:"meta,omitempty"` // 可选来源元信息；当前仅预留，不参与恢复语义
}

func validateCheckpointSchema(cp checkpoint) error {
	if cp.Schema > CurrentCheckpointSchema {
		return fmt.Errorf("loom: unsupported checkpoint schema version %d (supported version %d)",
			cp.Schema, CurrentCheckpointSchema)
	}
	return nil
}

// CheckpointInfo is the public metadata view of one historical checkpoint.
// CheckpointInfo 是单条历史检查点的公开元数据视图：不暴露完整 State，供列表与审计使用。
type CheckpointInfo struct {
	Seq        int64     `json:"seq"`                   // 步序号：History 按此对应的零填充键升序返回
	LastStep   string    `json:"last_step"`             // 该快照完成（或暂停于）的步骤名
	YieldPhase string    `json:"yield_phase,omitempty"` // 暂停阶段；普通步骤完成快照为空
	SavedAt    time.Time `json:"saved_at"`              // 检查点成功写入 latest 前生成的时间戳
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

	// Load the global step budget exactly once, in context > persisted state > graph option order.
	// 全局预算只装载一次，优先级固定为 context > 持久化余额 > 图配置初值：父图 context
	// 中的共享计数器永远压过子图状态，避免产生第二个计数器；全新 context 的 Resume/ResumeAt
	// 则从 __budget_remaining 恢复余额而非初值，绝不因恢复而重置扩容。只有全新根图运行既无
	// context 计数器、状态也无余额时，才使用 g.stepBudget 配置初值。
	if _, ok := ctx.Value(budgetKey{}).(*atomic.Int64); !ok {
		var initial int64
		var fromState bool
		switch value := input["__budget_remaining"].(type) {
		case int64:
			initial, fromState = value, true
		case int:
			initial, fromState = int64(value), true
		case float64:
			initial, fromState = int64(value), true
		}
		if fromState {
			remaining := atomic.Int64{}
			remaining.Store(initial)                              // 恢复检查点余额；0 也是有效值，下一步立即 StopBudget
			ctx = context.WithValue(ctx, budgetKey{}, &remaining) // 存指针：后续父子图递减同一个计数器
		} else if g.stepBudget != nil {
			remaining := atomic.Int64{}
			remaining.Store(*g.stepBudget)                        // 全新根图才装入配置初值
			ctx = context.WithValue(ctx, budgetKey{}, &remaining) // 存指针：所有子图递减的是同一个计数器
		}
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
// 兼容差异：Resume 对空 phase 按 mid_step 处理，因为 v1.0 latest 表示“暂停中”的恢复点；
// ResumeAt 则对空 phase 按 after_step 处理，因为历史条目是步骤完成后的快照，重跑会造成双重作用。
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
	if err := validateCheckpointSchema(cp); err != nil {
		return nil, fmt.Errorf("loom: cannot resume run %s: %w", runID, err)
	}

	// 恢复现场：以检查点快照为底、人工输入为增量做合并——人工反馈按正常合并语义并入状态；
	// 随快照落盘的 __budget_remaining 会在下方 Run 中为全新 context 重新装载共享计数器。
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

// ResumeAt forks a new run from an immutable historical checkpoint.
// ResumeAt 从任意历史检查点分叉出全新运行：读取源快照但只向新 UUID 写检查点，源存档树绝不改写。
// 与 Resume 的 v1.0 兼容语义不同，空 yield phase 在这里按 after_step 处理；历史条目记录的是步骤完成
// 后的状态，若按 mid_step 重跑原步骤，外部副作用与状态增量都可能重复发生。fork 默认继承源快照
// 在分叉点的 __budget_remaining；宿主可在 input 中显式提供同名键，借由下方 Merge 覆盖该余额。
func (g *Graph) ResumeAt(ctx context.Context, runID string, seq int64, input State, store Store) (*RunResult, error) {
	historyKey := fmt.Sprintf("%s/%012d", runID, seq) // 零填充格式与 doCheckpoint 的历史键协议完全一致
	data, err := store.Get(ctx, "checkpoint:"+g.Name, historyKey)
	if err != nil {
		// Store 的具体未找到错误不稳定；对外统一包裹内核哨兵，调用方可用 errors.Is 分支。
		return nil, fmt.Errorf("loom: checkpoint not found for run %s at seq %d: %w (%v)",
			runID, seq, ErrCheckpointNotFound, err)
	}
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		// 历史键存在但内容不可解析即视为损坏；不得从不可信快照启动新分支。
		return nil, fmt.Errorf("loom: corrupt checkpoint for run %s at seq %d: %w (%v)",
			runID, seq, ErrCorruptCheckpoint, err)
	}
	if err := validateCheckpointSchema(cp); err != nil {
		return nil, fmt.Errorf("loom: cannot resume run %s at seq %d: %w", runID, seq, err)
	}

	// fork 现场以源快照为底合并调用方输入，然后强制覆盖三个身份/溯源协议键：
	// 新运行绝不复用源 runID；__seq 保留在合并状态中，使首次 fork 检查点从源 seq+1 续编。
	newID := uuid.New().String()
	state := cp.State.Merge(input, g.mergeConfig)
	state["__run_id"] = newID
	state["__parent_run"] = runID
	state["__parent_seq"] = seq

	if cp.YieldPhase == "mid_step" {
		// 显式 mid_step 表示步骤尚未完成：从原步骤重入，让它消费 fork 输入并重跑。
		return g.withEntry(cp.LastStep).Run(ctx, state, store)
	}

	// 其余 phase（包括空串与 after_step）均把历史节点视为“该步骤已完成”，直接执行其路由。
	router := g.routers[cp.LastStep]
	if router == nil {
		// 源节点没有出边即为终端：fork 已创建身份，但没有新步骤需要执行，因此不产生新检查点。
		return &RunResult{
			State: state, LastStep: cp.LastStep, RunID: newID, StopReason: StopCompleted,
		}, nil
	}
	nextStep, err := router(ctx, state)
	if err != nil {
		return &RunResult{
			State: state, LastStep: cp.LastStep, RunID: newID, StopReason: StopError,
		}, err
	}
	if nextStep == "" {
		// 路由器显式返回空串同样是正常完成，不执行任何源步骤，也不落 fork 检查点。
		return &RunResult{
			State: state, LastStep: cp.LastStep, RunID: newID, StopReason: StopCompleted,
		}, nil
	}
	return g.withEntry(nextStep).Run(ctx, state, store) // 后续所有写入由新 __run_id 隔离到 fork 存档树
}

// History lists valid historical checkpoint metadata for a run in ascending step order.
// History 列出某次运行的有效历史元数据：依赖 Store.List 的字典序契约与零填充键，天然按步升序返回。
// 单条键读取失败或 JSON 损坏只记 Warn 并跳过，不能让局部坏档阻断其余历史的审计与恢复。
func (g *Graph) History(ctx context.Context, store Store, runID string) ([]CheckpointInfo, error) {
	if store == nil {
		return nil, fmt.Errorf("loom: checkpoint history requires a non-nil store")
	}
	ns := "checkpoint:" + g.Name
	keys, err := store.List(ctx, ns, runID+"/")
	if err != nil {
		return nil, fmt.Errorf("loom: list checkpoint history for run %s: %w", runID, err)
	}

	history := make([]CheckpointInfo, 0, len(keys))
	for _, key := range keys {
		data, err := store.Get(ctx, ns, key)
		if err != nil {
			// List 与 Get 之间可能发生并发淘汰；把这种单条读取失败按损坏条目同样尽力跳过。
			slog.Warn("loom: checkpoint history entry unreadable",
				"graph", g.Name, "run_id", runID, "key", key, "error", err)
			continue
		}
		var cp checkpoint
		if err := json.Unmarshal(data, &cp); err != nil {
			slog.Warn("loom: corrupt checkpoint history entry skipped",
				"graph", g.Name, "run_id", runID, "key", key, "error", err)
			continue
		}
		if err := validateCheckpointSchema(cp); err != nil {
			return nil, fmt.Errorf("loom: cannot read checkpoint history for run %s at key %s: %w", runID, key, err)
		}
		history = append(history, CheckpointInfo{
			Seq: cp.Seq, LastStep: cp.LastStep, YieldPhase: cp.YieldPhase, SavedAt: cp.SavedAt,
		})
	}
	return history, nil
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

	// 序号由 State 携带而非 Graph 内存字段，因而 JSON Resume 与 ResumeAt fork 都会自动续编；
	// int/float64 兼容调用方直接构造状态及 JSON 往返后数字被解码成 float64 的两种来源。
	var seq int64
	switch value := state["__seq"].(type) {
	case int64:
		seq = value
	case int:
		seq = int64(value)
	case float64:
		seq = int64(value)
	}
	seq++
	state["__seq"] = seq // 先写回状态再序列化，确保 cp.State 与 cp.Seq 始终一致
	// 预算在步骤执行前已扣减；仅当本次运行确有共享计数器时才把扣减后余额写入同一份快照。
	// 未启用预算的图不创建协议键，保持既有状态与检查点零污染。
	if budget, ok := ctx.Value(budgetKey{}).(*atomic.Int64); ok {
		state["__budget_remaining"] = budget.Load()
	}

	// fork 溯源键随 State 跨后续 Resume 传播；ParentSeq 同样兼容 JSON 的 float64 数字表示。
	parentRun, _ := state["__parent_run"].(string)
	var parentSeq int64
	switch value := state["__parent_seq"].(type) {
	case int64:
		parentSeq = value
	case int:
		parentSeq = int64(value)
	case float64:
		parentSeq = int64(value)
	}

	// 组装检查点：完整状态 + 序号/溯源 + 步骤位置 + 暂停阶段 + 时间戳，构成可独立恢复的自洽快照。
	cp := checkpoint{
		Schema:     CurrentCheckpointSchema,
		RunID:      runID,
		Graph:      g.Name,
		Seq:        seq,
		ParentRun:  parentRun,
		ParentSeq:  parentSeq,
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

	// historyKeep=0 保持原有 latest-only 行为：不创建任何 runID/ 前缀键。
	if g.historyKeep == 0 {
		return nil
	}
	historyKey := fmt.Sprintf("%s/%012d", runID, seq) // 固定宽度使 List 字典序与步序完全一致
	if err := store.Put(ctx, "checkpoint:"+g.Name, historyKey, data); err != nil {
		// 历史是审计增强而非关键恢复指针：失败一律 Warn，CheckpointRequired 也不得因此中止运行。
		slog.Warn("loom: checkpoint history write failed (best-effort)",
			"graph", g.Name, "run_id", runID, "seq", seq, "error", err)
		return nil
	}

	// 正数保留窗口在成功追加后淘汰恰好滑出窗口的旧键；Delete 幂等且失败不影响 latest/新历史。
	if g.historyKeep > 0 {
		evictSeq := seq - int64(g.historyKeep)
		if evictSeq >= 1 {
			evictKey := fmt.Sprintf("%s/%012d", runID, evictSeq)
			if err := store.Delete(ctx, "checkpoint:"+g.Name, evictKey); err != nil {
				slog.Warn("loom: checkpoint history eviction failed (best-effort)",
					"graph", g.Name, "run_id", runID, "seq", evictSeq, "error", err)
			}
		}
	}
	return nil // latest 与本步历史均已落盘（淘汰为 best-effort）
}

// withEntry creates a shallow copy of the graph with a different entry point.
// The copy shares steps, routers, hooks, and mergeConfig but does NOT
// re-initialize the configured step budget (the balance comes from context or persisted state).
// withEntry 以不同入口浅拷贝出一个图，供 Resume 从任意步骤重入。
// 关键取舍：stepBudget 置 nil 而非复制——同一调用链从父图 context 继承原计数器；跨进程或
// 新请求的 Resume/ResumeAt 则由检查点状态恢复余额。若拷贝配置初值，缺少余额的旧快照可能
// 在恢复时重新扩容，等于绕过全局熔断；置 nil 与“恢复余额而非初值”的协议共同防止重置额度。
func (g *Graph) withEntry(newEntry string) *Graph {
	return &Graph{
		Name:             g.Name,             // 图名不变：检查点仍写同一命名空间
		steps:            g.steps,            // 共享步骤表（运行期只读，无需深拷贝）
		routers:          g.routers,          // 共享路由表
		entry:            newEntry,           // 唯一的差异：新入口
		hooks:            g.hooks,            // 共享钩子
		mergeConfig:      g.mergeConfig,      // 共享已冻结的合并配置，保证合并语义一致
		checkpointPolicy: g.checkpointPolicy, // 沿用检查点失败策略
		historyKeep:      g.historyKeep,      // 沿用历史保留策略，Resume/fork 后继续追加同一配置的链
		maxIter:          g.maxIter,          // 沿用单图熔断上限（Resume 后步数从 0 重新计）
		stepBudget:       nil,                // 刻意不复制初值：预算经 context 继承或从快照余额恢复，禁止重置扩容
	}
}
