package loom

import (
	"context" // 路由函数签名携带 context 以便取消传播（内置路由器本身不使用）
	"fmt"     // Branch 用 fmt.Sprintf 把非字符串的状态值兜底转成字符串
)

// Router determines the next step based on current state.
// Returns the name of the next step, or "" to halt.
// Router（路由器）在每个步骤之后决定下一跳：输入当前状态，返回下一个步骤名。
// 核心协议：返回空字符串 "" 表示停机（图正常结束）。
// 设计立场：路由是确定性代码而非模型决策——LLM 只产出状态内容，走哪条边由代码裁决，
// 因此控制流可测试、可回放、与模型输出解耦。
type Router func(ctx context.Context, state State) (string, error)

// Edge describes a possible transition from one step to another (for topology export).
// Edge 描述一条可能的转移边，仅用于拓扑导出/可视化，不参与实际路由决策。
type Edge struct {
	// To：目标步骤名；空串 "" 表示终点，与路由器“返回空串即停机”的协议一致。
	To string `json:"to"` // target step name ("" = end)
	// Label：该边的条件标签（例如分支取值），仅供可视化展示。
	Label string `json:"label,omitempty"` // condition label
}

// Always returns the same next step.
// Always 构造无条件路由器：无论状态如何都固定跳到 next，用于线性流水线的顺序衔接。
func Always(next string) Router {
	return func(_ context.Context, _ State) (string, error) { return next, nil } // 忽略上下文与状态，恒定返回 next
}

// End always halts the graph.
// End 构造终止路由器：恒定返回空字符串，按协议表示停机（图正常完成）。
func End() Router {
	return func(_ context.Context, _ State) (string, error) { return "", nil } // 空串即停机信号
}

// Branch routes based on a state key's string value.
// Branch 按状态中 key 对应值的字符串形式查表路由：命中 routes 走对应步骤，未命中走 fallback。
func Branch(key string, routes map[string]string, fallback string) Router {
	return func(_ context.Context, s State) (string, error) {
		var val string // 归一化后的路由键值
		// 类型归一化：State 经 JSON 往返后值类型可能漂移（数字变 float64 等），统一转成字符串再查表。
		switch v := s[key].(type) {
		case string:
			val = v // 常规情况：直接使用字符串值
		case nil:
			val = "" // 键不存在（或显式为 nil）：按空串处理，routes 里可用 "" 显式接住这种情况
		default:
			val = fmt.Sprintf("%v", v) // 其他类型（数字/布尔等）兜底格式化，保证类型漂移不改变路由行为
		}
		// 查路由表：命中即转移到对应步骤。
		if next, ok := routes[val]; ok {
			return next, nil
		}
		return fallback, nil // 未命中走兜底目标；fallback 传 "" 即“未匹配则停机”
	}
}

// BranchFunc routes based on a user-supplied key extractor.
// BranchFunc 是 Branch 的泛化版本：路由键由调用方提供的提取函数计算，
// 适合键值需要组合多个状态字段或额外加工的场景。
func BranchFunc(extract func(State) string, routes map[string]string, fallback string) Router {
	return func(_ context.Context, s State) (string, error) {
		val := extract(s) // 由调用方逻辑从状态中计算路由键
		// 查表命中则转移到对应步骤。
		if next, ok := routes[val]; ok {
			return next, nil
		}
		return fallback, nil // 未命中走兜底目标（"" 即停机）
	}
}

// Condition routes based on a predicate.
// Condition 构造二元条件路由器：谓词为真走 ifTrue、为假走 ifFalse，即最简单的 if/else 分支。
func Condition(pred func(State) bool, ifTrue, ifFalse string) Router {
	return func(_ context.Context, s State) (string, error) {
		// 用谓词判定当前状态，决定走真分支还是假分支。
		if pred(s) {
			return ifTrue, nil // 条件成立：转移到 ifTrue（"" 表示停机）
		}
		return ifFalse, nil // 条件不成立：转移到 ifFalse（"" 表示停机）
	}
}
