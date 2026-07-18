package loom

import "errors" // 标准库 errors：定义哨兵错误，调用方用 errors.Is 精确匹配而非解析字符串

// 包级哨兵错误：内核所有可预期的失败模式收敛为这几种固定值，
// 供调用方（含 Store 实现方）做程序化分支处理。
var (
	ErrMaxIterations      = errors.New("loom: max iterations exceeded")           // 单图迭代熔断：本图步数超过 maxIter 上限（防路由环路死循环）
	ErrBudgetExhausted    = errors.New("loom: global step budget exhausted")      // 全局预算耗尽：父图与所有子图共享的步数预算被扣完
	ErrStepNotFound       = errors.New("loom: step not found")                    // 拓扑错误：路由到了图中未注册的步骤名
	ErrCheckpointNotFound = errors.New("loom: checkpoint not found")              // 恢复失败：按 runID 找不到检查点（从未运行或已被清理）
	ErrCorruptCheckpoint  = errors.New("loom: corrupt checkpoint")                // 恢复失败：检查点存在但 JSON 反序列化失败（数据损坏/版本不兼容）
	ErrNestedTx           = errors.New("loom: nested transactions not supported") // 协议约束：Store.Tx 内再开事务必须返回此错误，禁止静默的伪嵌套
)
