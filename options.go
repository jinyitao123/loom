package loom

import "context" // StepHook 签名需要 context，与步骤执行共享同一条取消链

// CheckpointPolicy controls how checkpoint failures are handled.
// CheckpointPolicy（检查点策略）决定检查点落盘失败时引擎的取舍：
// 是“尽力而为、失败只告警继续跑”，还是“必须落盘、失败即中止”。
type CheckpointPolicy int

const (
	// CheckpointBestEffort logs checkpoint failures but does not abort.
	// 尽力而为策略（默认）：检查点写失败仅记 Warn 日志、执行继续。
	// 取舍：优先保证运行可用性，代价是失败后若进程崩溃将无法从最新进度恢复。
	CheckpointBestEffort CheckpointPolicy = iota
	// CheckpointRequired aborts the graph if checkpoint fails.
	// 强制落盘策略：检查点写失败立即中止整个图。
	// 取舍：优先保证可恢复性（每一步都有可靠快照），适合 HITL 等必须能恢复的场景。
	CheckpointRequired
)

// StepHook runs before or after each step execution.
// StepHook（步骤钩子）在每个步骤执行前/后被调用，承载日志、鉴权、指标等横切逻辑；
// 返回非 nil error 会以 StopHookAbort 中止整个图——钩子对执行拥有否决权。
type StepHook func(ctx context.Context, stepName string, state State) error

// HookPoints holds before/after hooks for graph execution.
// HookPoints 汇集图执行的前置/后置钩子，各自按注册顺序依次执行。
type HookPoints struct {
	Before []StepHook // 前置钩子：在步骤执行前运行，任一报错则该步骤不会被执行
	After  []StepHook // 后置钩子：在步骤增量合并进状态之后运行，可观察到合并后的完整状态
}

// GraphOption configures a Graph at construction time.
// GraphOption 是构造期配置函数（函数式选项模式）：只在 NewGraph 时生效，图建成后配置即定型。
type GraphOption func(*Graph)

// WithMergeConfig 指定图的合并配置；注意 NewGraph 会随即将其冻结（frozen），运行期不可再改策略。
func WithMergeConfig(cfg *MergeConfig) GraphOption {
	return func(g *Graph) { g.mergeConfig = cfg } // 仅挂载引用，冻结动作由 NewGraph 统一执行
}

// WithCheckpointPolicy 设置检查点失败策略（默认 CheckpointBestEffort）。
func WithCheckpointPolicy(p CheckpointPolicy) GraphOption {
	return func(g *Graph) { g.checkpointPolicy = p } // 覆盖默认的尽力而为策略
}

// WithCheckpointHistory configures optional per-step checkpoint history retention.
// WithCheckpointHistory(keep) 配置可选的按步检查点历史：0=关闭（默认，保持 latest 覆盖写）；
// -1=全部保留；n>0=只保留最近 n 条。历史写入始终是尽力而为，不改变 checkpointPolicy。
func WithCheckpointHistory(keep int) GraphOption {
	return func(g *Graph) { g.historyKeep = keep } // 原样保存保留策略：doCheckpoint 据此决定追加与淘汰
}

// WithMaxIterations 设置单图迭代上限（per-graph 熔断，默认 100），防止路由环路导致死循环。
func WithMaxIterations(n int) GraphOption {
	return func(g *Graph) { g.maxIter = n } // 覆盖 NewGraph 里的默认值 100
}

// WithStepBudget sets a global step limit shared across parent and all sub-graphs.
// WithStepBudget 设置全局步数预算：与 maxIter 不同，它在根图 Run 时装入 atomic 计数器、
// 经 context 传递给所有子图共享，是第二层熔断——即使每个子图各自不超 maxIter，
// 全体累计步数也不能超出此预算。只应在根图上设置。
func WithStepBudget(n int64) GraphOption {
	return func(g *Graph) { g.stepBudget = &n } // 取参数 n 的地址存入：nil 表示未启用预算，非 nil 触发根图初始化计数器
}
