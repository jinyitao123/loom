package stdlib

import (
	"context"     // 钩子签名要求携带 context（本文件的钩子未使用它）
	"fmt"         // 超限时构造带具体数值的错误信息
	"sync"        // CostBudgetHook 用互斥锁保护 float64 累加
	"sync/atomic" // TokenBudgetHook 用原子加法做无锁累计

	"github.com/jinyitao123/loom"          // 内核包：StepHook 类型与 State
	"github.com/jinyitao123/loom/contract" // contract 包：Usage 用量类型
)

// CostBudgetHook returns an AfterStep hook that tracks cumulative cost.
// When the budget is exceeded, it returns an error that stops the graph.
// CostBudgetHook 返回一个 After-Step 钩子：每个步骤跑完后从状态里取 usage 累计成本。
// 一旦累计超过 maxUSD 上限即返回错误，引擎收到钩子错误后以 hook_abort 停机，
// 从机制上保证失控的 agent 不会无限烧钱。
func CostBudgetHook(maxUSD float64) loom.StepHook {
	// 累计值与锁被闭包捕获：一个钩子实例对应一份独立预算，跨步骤、跨并行分支共享。
	var totalCost float64
	var mu sync.Mutex
	return func(_ context.Context, _ string, state loom.State) error {
		// float64 的"读-加-写"不是原子操作，且读累计值与比较上限必须在同一临界区内，
		// 所以这里用互斥锁而非 atomic（Go 没有针对 float64 的原子加法原语）。
		mu.Lock()
		defer mu.Unlock()
		// 仅当状态里带有类型正确的 usage 时才累计；没跑 LLM 的步骤自然跳过。
		if usage, ok := state["usage"].(contract.Usage); ok {
			totalCost += usage.CostUSD // 累加 provider 计算好的本步成本
		}
		// 超限即报错：错误会被引擎当作 hook_abort 信号，终止整张图的执行。
		if totalCost > maxUSD {
			return fmt.Errorf("loom/budget: cost $%.4f exceeds limit $%.4f", totalCost, maxUSD)
		}
		// 预算内放行，图继续执行。
		return nil
	}
}

// TokenBudgetHook tracks cumulative input+output tokens.
// TokenBudgetHook 返回一个 After-Step 钩子：累计输入+输出 token 总量，
// 超过 maxTokens 即返回错误，同样触发引擎 hook_abort 停机。
func TokenBudgetHook(maxTokens int64) loom.StepHook {
	// token 累计值由闭包捕获；int64 可用 atomic 无锁累加，比互斥锁更轻。
	var totalTokens int64
	return func(_ context.Context, _ string, state loom.State) error {
		// 同样只在状态携带 usage 时统计。
		if usage, ok := state["usage"].(contract.Usage); ok {
			// atomic.AddInt64 一步完成"加并取回新值"：整数有原子原语可用（float64 没有），
			// 且返回值 current 就是本次累加后的快照，比较上限无需再加锁。
			current := atomic.AddInt64(&totalTokens, int64(usage.InputTokens+usage.OutputTokens))
			// 超限即报错停机；错误信息带上当前累计值便于定位烧量来源。
			if current > maxTokens {
				return fmt.Errorf("loom/budget: tokens %d exceeds limit %d", current, maxTokens)
			}
		}
		// 预算内放行。
		return nil
	}
}
