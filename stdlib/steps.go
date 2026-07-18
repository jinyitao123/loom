package stdlib

import (
	"context" // 步骤签名的第一参数，承载超时/取消并向子图透传
	"fmt"     // 子图暂停策略违规时构造错误

	"github.com/jinyitao123/loom" // 内核包：Step/State/Graph/Store/RunResult
)

// StepHook is a check function used by guard steps.
// StepHook 是守卫步骤使用的检查函数签名：拿到步骤名与当前状态做校验，
// 返回 error 即表示检查未通过。
type StepHook func(ctx context.Context, stepName string, state loom.State) error

// NewGuardStep creates a step that runs guardrail checks.
// NewGuardStep 把一组检查函数组合成一个守卫步骤：按顺序执行，第一条失败即短路。
func NewGuardStep(checks ...StepHook) loom.Step {
	return func(ctx context.Context, state loom.State) (loom.State, error) {
		// 顺序执行每条检查；任何一条不通过就立即终止。
		for _, check := range checks {
			if err := check(ctx, "guard", state); err != nil {
				// 检查失败：同时写入 __blocked 标记与原因（供路由器/审计读取），并把错误上抛。
				return loom.State{"__blocked": true, "__block_reason": err.Error()}, err
			}
		}
		// 全部通过：返回空状态增量，不改动任何数据。
		return loom.State{}, nil
	}
}

// NewHumanWaitStep creates a step that yields for human input.
// NewHumanWaitStep 创建一个等待人工输入的暂停步骤（HITL 人工介入）。
// 幂等设计是关键：暂停（yield）后引擎存检查点退出；恢复（resume）时本步骤会被
// 原样重跑——若发现人工回复已写入状态的 responseKey，就直接放行进入下一步。
// 也就是说"等待"与"放行"是同一段代码的两次执行，靠状态里有没有回复来区分，
// 步骤本身无需任何"我暂停过吗"的记忆。
func NewHumanWaitStep(promptKey, responseKey string) loom.Step {
	return func(_ context.Context, state loom.State) (loom.State, error) {
		// 第二次执行（恢复后）：回复已在状态中，说明人工输入已到位，直接放行。
		if _, ok := state[responseKey]; ok {
			return loom.State{}, nil
		}
		// 第一次执行：回复还不存在，发出暂停信号让引擎存检查点并挂起。
		// __yield_phase 用 mid_step 表示"步骤中途暂停"——恢复时要从本步骤重新执行
		// （以便走上面的放行分支），而不是跳到下一步。
		return loom.State{
			"__yield":       true,
			"__yield_phase": "mid_step",
		}, nil
	}
}

// NewLLMCallStep creates a single LLM call step (no tool loop).
// NewLLMCallStep 创建单次 LLM 调用步骤（不带工具循环）。
func NewLLMCallStep(llm interface {
	Chat(ctx context.Context, req interface{}) (interface{}, error)
}) loom.Step {
	// Placeholder — real implementation uses contract.LLM.
	// 占位实现：正式版本应改用 contract.LLM 接口；当前直接返回空状态增量。
	return func(ctx context.Context, state loom.State) (loom.State, error) {
		return loom.State{}, nil
	}
}

// YieldPolicy controls how a parent step handles a child graph's yield.
// YieldPolicy 决定父步骤如何处置子图发出的暂停信号，共三种策略（见下方常量）。
type YieldPolicy int

const (
	YieldBubble YieldPolicy = iota // 冒泡：把子图的暂停原样上抛给父图，父图跟着一起暂停（默认）
	YieldTrap                      // 拦截：不允许子图暂停，一旦发生视为错误
	YieldCustom                    // 自定义：交给调用方提供的 YieldHandler 决定如何处理
)

// SubGraphOpts configures sub-graph step behavior.
// SubGraphOpts 配置子图步骤的行为：暂停策略 + （YieldCustom 时的）自定义处理器。
type SubGraphOpts struct {
	YieldPolicy  YieldPolicy                                                                // 采用哪种暂停策略
	YieldHandler func(ctx context.Context, childResult *loom.RunResult) (loom.State, error) // YieldCustom 专用：自定义处置子图暂停结果
}

// NewSubGraphStep creates a step that runs a child graph.
// NewSubGraphStep 把整张子图包装成父图中的一个普通步骤——
// 这是"任何函数都能当步骤"理念的兑现：图的组合不需要引擎提供任何专门机制，
// 一个闭包就够了。子图有自己独立的运行与检查点，父图只看到一次步骤调用。
func NewSubGraphStep(child *loom.Graph, store loom.Store, opts ...SubGraphOpts) loom.Step {
	// 解析可选配置：默认策略是 YieldBubble（子图暂停则父图跟着暂停）。
	o := SubGraphOpts{YieldPolicy: YieldBubble}
	if len(opts) > 0 {
		o = opts[0]
	}

	return func(ctx context.Context, state loom.State) (loom.State, error) {
		// 以父图当前状态为输入，同步运行整张子图直到完成或暂停。
		result, err := child.Run(ctx, state, store)
		if err != nil {
			// 子图出错：若带有部分结果状态则一并上交（便于父图诊断），错误照常上抛。
			if result != nil {
				return result.State, err
			}
			return nil, err
		}

		// 子图没跑完而是暂停了（内部有步骤在等人工介入）：按策略处置。
		if result.Yielded {
			switch o.YieldPolicy {
			case YieldBubble:
				// 冒泡协议：父步骤自己也发出 mid_step 暂停，并把 __child_run_id / __child_graph
				// 记入状态——恢复时宿主凭这两个键找到暂停中的子图运行，先恢复子图，
				// 子图完成后父步骤重跑（幂等），层层向上传导。
				return loom.State{
					"__yield":        true,
					"__yield_phase":  "mid_step",
					"__child_run_id": result.RunID,
					"__child_graph":  child.Name,
				}, nil
			case YieldTrap:
				// 拦截策略：该子图被声明为"不允许暂停"，暂停即违约，直接报错。
				return nil, fmt.Errorf("loom: sub-graph %q yielded but YieldTrap is set", child.Name)
			case YieldCustom:
				// 自定义策略：交给调用方处理器全权决定（可翻译、可吞掉、可改写状态）。
				if o.YieldHandler != nil {
					return o.YieldHandler(ctx, result)
				}
				// 声明了 YieldCustom 却没给处理器：配置错误，显式报出。
				return nil, fmt.Errorf("loom: sub-graph %q yielded with YieldCustom but no handler", child.Name)
			}
		}

		// 子图正常完成：其最终状态就是本步骤的输出，合并回父图状态。
		return result.State, nil
	}
}

// ContextCompressor compresses conversation history for handoffs.
// ContextCompressor 是移交（handoff）前的上下文压缩抽象：
// 把冗长的对话历史压缩成目标 agent 需要的精华，避免整段上下文原样搬运。
type ContextCompressor interface {
	Compress(msgs []interface{}) []interface{}
}

// NewHandoffStep creates a step that delegates to another graph with context compression.
// NewHandoffStep 创建"压缩移交"步骤：把当前对话历史压缩后交给目标图接管处理，
// 本质上是 NewSubGraphStep 的特化——多了上下文压缩与身份键透传，暂停策略固定为冒泡。
func NewHandoffStep(target *loom.Graph, store loom.Store, compressor ContextCompressor) loom.Step {
	return func(ctx context.Context, state loom.State) (loom.State, error) {
		// 取出当前对话历史；取不到就按空历史移交（忽略错误是有意为之）。
		msgs, _ := GetMessages(state)
		// 转成 []interface{} 以适配 ContextCompressor 的通用签名。
		var rawMsgs []interface{}
		for _, m := range msgs {
			rawMsgs = append(rawMsgs, m)
		}
		// 执行压缩：只带走精华上下文，控制目标图的 token 开销。
		compressed := compressor.Compress(rawMsgs)
		// 为目标图构造全新的初始状态：只含压缩后的历史，不带父图的其他杂项。
		handoffState := loom.State{"messages": compressed}
		// 身份三键（会话/用户/命名空间）若存在则透传，保证目标图的存储隔离与归属一致。
		for _, k := range []string{"session_id", "user_id", "namespace"} {
			if v, ok := state[k]; ok {
				handoffState[k] = v
			}
		}
		// 同步运行目标图直到完成或暂停。
		result, err := target.Run(ctx, handoffState, store)
		if err != nil {
			// 出错处理与子图步骤一致：部分状态随错误一并上交。
			if result != nil {
				return result.State, err
			}
			return nil, err
		}

		// 目标图暂停：走与 YieldBubble 相同的冒泡协议（__child_run_id/__child_graph），
		// 额外记录 handoff_to 标明这是一次移交而非普通子图调用。
		if result.Yielded {
			return loom.State{
				"__yield":        true,
				"__yield_phase":  "mid_step",
				"__child_run_id": result.RunID,
				"__child_graph":  target.Name,
				"handoff_to":     target.Name,
			}, nil
		}

		// 目标图完成：只回收其最终输出与移交去向，不把目标图的内部状态灌回父图。
		return loom.State{
			"output":     result.State["output"],
			"handoff_to": target.Name,
		}, nil
	}
}
