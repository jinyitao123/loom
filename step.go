package loom

import "context" // 步骤签名携带 context，供取消与超时沿调用链传播

// Step is the atomic unit of execution.
// It receives the current state and returns the delta to merge.
// Returning a non-nil error aborts the graph.
// Step（步骤）是引擎的最小执行单元：输入当前状态（State），返回“状态增量”而非完整状态。
// 核心协议：步骤只产出增量，合并由引擎统一调用 State.Merge 完成——这保证每个检查点都是
// 合并后的自洽快照，也让恢复（resume）时重跑步骤是安全的。
// 返回非 nil error 会中止整个图：引擎会把错误现场写入 __error / __failed_step 协议键，
// 并尽力落一次检查点保存现场。
type Step func(ctx context.Context, state State) (State, error)
