package loom

import "encoding/json" // State 的序列化协议就是 JSON：检查点落盘与跨进程传输都走它

// State is the execution context shared across all steps.
// Keys are strings. Values are any JSON-serializable type.
// State（状态）是贯穿所有步骤的共享执行上下文：键为字符串、值为任意可 JSON 序列化类型的 map。
// 约束：值必须能过 JSON 往返（检查点依赖 Marshal），塞入不可序列化的值会在落盘时报错；
// 双下划线前缀键（__run_id、__yield、__yield_phase、__error、__failed_step）为引擎协议键，业务侧不应占用。
type State map[string]any

// Marshal 把状态序列化为 JSON 字节串，是检查点落盘所用的编码格式。
func (s State) Marshal() ([]byte, error) { return json.Marshal(s) }

// UnmarshalState 从 JSON 字节串还原状态；注意 JSON 往返后数值统一变为 float64、数组变为 []any。
func UnmarshalState(b []byte) (State, error) {
	var s State                  // 反序列化目标；解码失败时保持 nil 零值
	err := json.Unmarshal(b, &s) // 就地解码
	return s, err                // 状态与错误一并返回，由调用方判错
}

// MergePolicy defines how a specific key is merged.
// MergePolicy（合并策略）定义单个键的合并规则：输入旧值与新值，返回合并结果。
// 策略只关心“怎么合”，“何时合”由引擎在每个步骤之后统一驱动。
type MergePolicy func(existing, incoming any) any

// Built-in policies.
// 内置合并策略：覆盖最常见的四种合并语义。
// 各策略均使用宽容的类型断言（断言失败取零值），保证 JSON 往返后的类型漂移不会引发 panic。
var (
	// Overwrite：新值直接覆盖旧值，是最朴素也是默认的合并语义。
	Overwrite MergePolicy = func(_, incoming any) any { return incoming }

	// AppendSlice：把新切片追加到旧切片之后，典型用于 messages 这类只增不减的会话历史。
	AppendSlice MergePolicy = func(existing, incoming any) any {
		e, _ := existing.([]any) // 旧值断言为 []any；断言失败（含 nil）得空切片，等价于从头累积
		i, _ := incoming.([]any) // 新值同理；JSON 反序列化出的数组恰好就是 []any
		return append(e, i...)   // 顺序追加，保留时间序
	}

	// SumInt：整型累加，适合进程内计数器类键（如重试次数）。
	SumInt MergePolicy = func(existing, incoming any) any {
		e, _ := existing.(int) // 非 int（含 nil）按 0 处理；注意经 JSON 往返后数值变 float64，此策略不适合跨检查点使用
		i, _ := incoming.(int) // 同上
		return e + i           // 返回累加和
	}

	// SumFloat：浮点累加，适合成本、token 用量等度量；JSON 反序列化的数字默认就是 float64，可安全跨检查点累加。
	SumFloat MergePolicy = func(existing, incoming any) any {
		e, _ := existing.(float64) // 非 float64（含 nil）按 0 处理
		i, _ := incoming.(float64) // 同上
		return e + i               // 返回累加和
	}
)

// MergeConfig holds per-key merge policies.
// It is mutable during construction and frozen once passed to a Graph.
// MergeConfig（合并配置）保存“键 → 合并策略”的映射及兜底策略。
// 生命周期协议：构造期可随意 Register；一旦挂到 Graph 上即被冻结——
// 原因是配置可能被多个图共享，且运行中改策略会让同一图内不同时刻的合并语义不一致，
// 破坏检查点的可重放性。
type MergeConfig struct {
	policies map[string]MergePolicy // 按键精确匹配的策略表
	fallback MergePolicy            // 未注册键的兜底策略（默认 Overwrite）
	frozen   bool                   // 冻结标记：NewGraph 挂载时置 true，此后 Register 直接 panic
}

// NewMergeConfig 创建一份空白配置：无按键策略、兜底为 Overwrite。
func NewMergeConfig() *MergeConfig {
	return &MergeConfig{
		policies: make(map[string]MergePolicy), // 预建空表，Register 无需判 nil
		fallback: Overwrite,                    // 未注册的键默认整值覆盖
	}
}

// Register 为指定键注册合并策略；只允许在配置尚未挂到 Graph（未冻结）时调用。
func (mc *MergeConfig) Register(key string, policy MergePolicy) {
	// 冻结后禁止修改：直接 panic 把编程错误暴露在开发期，而不是让运行期合并语义悄悄漂移。
	if mc.frozen {
		panic("loom: cannot Register on a frozen MergeConfig (already attached to a Graph)")
	}
	mc.policies[key] = policy // 同键重复注册以最后一次为准
}

// DefaultMergeConfig returns stdlib's recommended defaults.
// DefaultMergeConfig 返回 stdlib 推荐默认值：messages 键用 AppendSlice 累积会话历史，其余键覆盖。
func DefaultMergeConfig() *MergeConfig {
	mc := NewMergeConfig()               // 以空白配置为底
	mc.Register("messages", AppendSlice) // 会话消息只增不减：每步返回的新消息追加到历史尾部
	return mc
}

// Merge applies the update to the current state using registered policies.
// Merge 把步骤返回的增量 update 按策略并入当前状态，并且每次都返回一个全新 map、绝不原地修改。
// 这是内核的核心不变量：步骤只产出增量、引擎负责合并，旧状态保持不可变——
// 由此每个检查点都是自洽快照，恢复重放与并发读取都不会被后续修改污染。
func (s State) Merge(update State, cfg *MergeConfig) State {
	// 未提供配置时构造临时兜底配置：全部键按 Overwrite 处理。
	if cfg == nil {
		cfg = &MergeConfig{fallback: Overwrite}
	}
	result := make(State, len(s)+len(update)) // 新建结果 map，容量按两者之和预估，避免中途扩容
	// 第一遍：把旧状态整体浅拷贝进结果（值本身不深拷贝，约定步骤不就地修改旧值的内部结构）。
	for k, v := range s {
		result[k] = v
	}
	// 第二遍：逐键应用增量——有注册策略用注册策略，否则用兜底策略（默认覆盖）。
	for k, v := range update {
		if policy, ok := cfg.policies[k]; ok {
			result[k] = policy(result[k], v) // 按键定制的合并（如 messages 追加）
		} else {
			result[k] = cfg.fallback(result[k], v) // 兜底合并（默认新值覆盖旧值）
		}
	}
	return result // 返回全新快照，原状态 s 保持不变
}
