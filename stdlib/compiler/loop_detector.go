package compiler

import (
	"context"       // 钩子签名要求的上下文参数（本检测器不使用其取消语义）
	"crypto/sha256" // 对"工具名+参数"取稳定哈希作为调用签名
	"fmt"           // 格式化签名与构造拦截错误
	"sync"          // 互斥锁：钩子可能被并发调用，历史环形缓冲需要保护

	"github.com/jinyitao123/loom/contract" // 只依赖契约层的 ToolCall/ToolHook 类型
)

// loopDetector tracks recent tool call signatures and blocks when the same
// call (name + args hash) repeats consecutively beyond a threshold. This
// prevents LLMs from getting stuck in retry loops. Ported from the archived
// weave compiler (zero platform coupling — depends only on contract).
//
// 中文说明：防重复护栏。以 ToolHook（钩子）形式挂在 chat 步骤的工具循环上，
// 追踪最近的工具调用签名（工具名 + 参数哈希），当同一签名"连续"重复达到
// 阈值时，在执行前拦截该调用并把错误作为工具结果回传给模型，提示其换思路
// ——循环不中断，模型有机会自我纠正。
//
// 与 toolloop 内建检测的分工：toolloop 对"一轮里的整批工具调用"做批次哈希，
// 整批连续相同达阈值时直接跳出循环（硬熔断，粒度粗）；本检测器工作在单个
// 调用粒度、跨批次计数，且是可插拔的软干预（回传错误而非终止），两者互补。
type loopDetector struct {
	mu        sync.Mutex // 保护下面全部字段的互斥锁
	threshold int        // consecutive repeats before blocking
	// 连续重复多少次后开始拦截（中文补充 threshold 字段语义）。
	history []string // ring buffer of call signatures
	// 签名历史的环形缓冲区，容量 threshold+1（中文补充 history 字段语义）。
	pos  int // write position
	size int // current fill level
}

// newLoopDetector 构造检测器；threshold 非正数时回退到缺省阈值 5，
// 保证调用方传 0 或负数也能得到可用的护栏而不是除零/空缓冲。
func newLoopDetector(threshold int) *loopDetector {
	if threshold <= 0 {
		threshold = 5 // 缺省阈值：连续 5 次相同调用即拦截
	}
	return &loopDetector{
		threshold: threshold,
		history:   make([]string, threshold+1), // track threshold+1 to detect threshold repeats
		// 缓冲容量取 threshold+1：环形写入下要判定"最近 threshold 次全部相同"，
		// 至少需要能完整保留 threshold 条历史再多一格写入位。
	}
}

// signature returns a stable hash of tool name + args.
// 中文说明：调用签名 = sha256("工具名:原始参数串") 的前 8 字节十六进制。
// 参数串不做归一化——只有字节级完全相同的调用才被视为"同一调用"，
// 宁可漏报也不误伤参数略有变化的合理重试。
func signature(call contract.ToolCall) string {
	h := sha256.Sum256([]byte(call.Name + ":" + call.Args)) // 名字与参数用 ':' 拼接后整体哈希
	return fmt.Sprintf("%x", h[:8])                         // 截取 8 字节足以避免碰撞，缩短存储
}

// record adds a call signature to the history.
// 中文说明：把一条签名写入环形缓冲。由 Post 钩子调用；注意工具循环对每个
// 调用（包括被 Pre 拦截的）都会执行 Post 钩子，故被拦截的重复调用同样入账
// ——模型若无视拦截错误继续发同一调用，会被持续拦截而非重新计数。
func (d *loopDetector) record(sig string) {
	d.mu.Lock() // 加锁：写位置与填充量必须原子更新
	defer d.mu.Unlock()
	d.history[d.pos] = sig               // 写入当前位置
	d.pos = (d.pos + 1) % len(d.history) // 写指针环形前进
	if d.size < len(d.history) {         // 未写满前记录实际填充量
		d.size++
	}
}

// isLoop checks if the last `threshold` entries all match the given signature.
// 中文说明：判定"最近 threshold 条历史是否全部等于给定签名"。注意判定的是
// "历史 + 即将执行的这次"构成连续重复——历史里已有 threshold 次相同，
// 第 threshold+1 次到来时即触发拦截。
func (d *loopDetector) isLoop(sig string) bool {
	d.mu.Lock() // 加锁：读取期间历史不能被并发写入
	defer d.mu.Unlock()
	// 历史条数还不足 threshold 时不可能构成循环，直接放行。
	if d.size < d.threshold {
		return false
	}
	// 从最新一条起往回逐条比对 threshold 条；任何一条不同即非循环。
	for i := 0; i < d.threshold; i++ {
		// 环形下标换算：pos 指向"下一个写入位"，pos-1 才是最新一条；
		// 加 len 再取模，避免 Go 负数取模得到负下标。
		idx := (d.pos - 1 - i + len(d.history)) % len(d.history)
		if d.history[idx] != sig {
			return false
		}
	}
	return true // threshold 条全部相同 → 判定为循环
}

// hook returns a ToolHook that detects and breaks tool call loops.
// 中文说明：把检测器打包成一个 ToolHook 挂到工具循环上。协议是：
// Pre 在工具执行前检查，判定为循环则返回错误——工具循环会把它包装成
// IsError 的工具结果（"blocked by hook: …"）回传给模型，而不是终止循环；
// Post 在工具实际执行后把本次签名记入历史。
func (d *loopDetector) hook() contract.ToolHook {
	return contract.ToolHook{
		// Pre 钩子：执行前拦截。未设置 Matcher，故对所有工具生效。
		Pre: func(_ context.Context, call contract.ToolCall) (contract.ToolCall, error) {
			sig := signature(call) // 计算本次调用的签名
			// 若最近 threshold 次全是同一签名，则拒绝本次（第 threshold+1 次）调用；
			// 错误文案直接面向模型，明确建议"换一种方式"。
			if d.isLoop(sig) {
				return call, fmt.Errorf("tool loop detected: %q called %d consecutive times with identical arguments — try a different approach", call.Name, d.threshold)
			}
			return call, nil // 未触发循环：原样放行，不修改调用
		},
		// Post 钩子：每个调用（含被拦截的）执行后记账，维持连续重复的计数。
		Post: func(_ context.Context, call contract.ToolCall, _ *contract.ToolResult) error {
			d.record(signature(call)) // 记录签名，供后续判定
			return nil                // 记账永不失败，不影响工具结果
		},
	}
}
