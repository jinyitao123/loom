package stdlib

import (
	"bytes"
	"context" // 步骤签名的第一参数，承载超时/取消并向子图透传
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt" // 子图暂停策略违规时构造错误
	"io"
	"math"
	"strings"

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

// ChildLifecycleObserver observes one completed child Run or Resume attempt
// before NewSubGraphStep projects the result into parent state.
type ChildLifecycleObserver func(
	ctx context.Context,
	result *loom.RunResult,
	runErr error,
	resumed bool,
) error

// SubGraphOpts configures sub-graph step behavior.
// SubGraphOpts 配置子图步骤的行为：暂停策略 + （YieldCustom 时的）自定义处理器。
type SubGraphOpts struct {
	YieldPolicy       YieldPolicy                                                                // 采用哪种暂停策略
	YieldHandler      func(ctx context.Context, childResult *loom.RunResult) (loom.State, error) // YieldCustom 专用：自定义处置子图暂停结果
	ParentMergeConfig *loom.MergeConfig                                                          // 父图应用本步骤增量时使用的合并配置
	ChildObserver     ChildLifecycleObserver
	LifecycleHooks    *loom.LifecycleHooks
}

// WithParentMergeConfig configures a sub-graph step to calculate deltas for
// the merge policies used by its parent graph.
func WithParentMergeConfig(cfg *loom.MergeConfig) SubGraphOpts {
	return SubGraphOpts{ParentMergeConfig: cfg}
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
		// 以父图当前状态为首次输入；重入时共享 continuation helper 恢复原 child run。
		result, resumed, runErr := runChildContinuation(
			ctx,
			state,
			child,
			store,
			o.LifecycleHooks,
			func() loom.State { return state },
		)
		if result != nil && o.ChildObserver != nil {
			observerErr := o.ChildObserver(ctx, result, runErr, resumed)
			if observerErr != nil {
				wrapped := fmt.Errorf(
					"loom: sub-graph %q child observer: %w",
					child.Name,
					observerErr,
				)
				if runErr != nil {
					return nil, errors.Join(runErr, wrapped)
				}
				return nil, wrapped
			}
		}
		if runErr != nil {
			if result != nil {
				return result.State, runErr
			}
			return nil, runErr
		}

		// 子图没跑完而是暂停了（内部有步骤在等人工介入）：按策略处置。
		if result.Yielded {
			switch o.YieldPolicy {
			case YieldBubble:
				return childYieldUpdate(child, result, resumed)
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

		// 子图正常完成：只把相对步骤输入发生变化的键作为增量交给父引擎合并。
		delta, err := subGraphDelta(state, result.State, o.ParentMergeConfig)
		if err != nil {
			return nil, err
		}
		if resumed {
			return withChildContinuationCleanup(delta), nil
		}
		return delta, nil
	}
}

type childCheckpoint struct {
	RunID      string     `json:"run_id"`
	Graph      string     `json:"graph"`
	Seq        int64      `json:"seq"`
	YieldPhase string     `json:"yield_phase"`
	State      loom.State `json:"state"`
}

func runChildContinuation(
	ctx context.Context,
	parent loom.State,
	child *loom.Graph,
	store loom.Store,
	lifecycleHooks *loom.LifecycleHooks,
	initial func() loom.State,
) (*loom.RunResult, bool, error) {
	runID, runPresent := parent["__child_run_id"]
	graphName, graphPresent := parent["__child_graph"]
	resumeRaw, inputPresent := parent["__child_resume_input"]
	tokenRaw, tokenPresent := parent["__child_yield_token"]
	seqRaw, seqPresent := parent["__child_checkpoint_seq"]
	revisionRaw, revisionPresent := parent["__child_graph_revision"]
	hasContinuationKey := runPresent || graphPresent || tokenPresent || seqPresent || revisionPresent

	run, runOK := runID.(string)
	graph, graphOK := graphName.(string)
	token, tokenOK := tokenRaw.(string)
	seq, seqOK := protocolInt64(seqRaw)
	revision, revisionOK := revisionRaw.(string)
	complete := runPresent && graphPresent && tokenPresent && seqPresent && revisionPresent &&
		runOK && graphOK && tokenOK && seqOK && revisionOK &&
		run != "" && graph != "" && token != "" && seq > 0 && revision != ""
	if !complete {
		if hasContinuationKey || inputPresent {
			return nil, false, fmt.Errorf("loom: child graph %q continuation is missing or invalid", child.Name)
		}
		var result *loom.RunResult
		var err error
		if lifecycleHooks == nil {
			result, err = child.Run(ctx, initial(), store)
		} else {
			result, err = child.RunWithLifecycle(ctx, initial(), store, *lifecycleHooks)
		}
		if err != nil {
			return result, false, fmt.Errorf("loom: child graph %q run: %w", child.Name, err)
		}
		return result, false, nil
	}
	if graph != child.Name {
		return nil, false, fmt.Errorf("loom: child graph continuation names %q, current graph is %q", graph, child.Name)
	}
	if revision != childGraphRevision(child) {
		return nil, false, fmt.Errorf("loom: child graph %q run %q stale continuation: topology revision does not match current child graph", child.Name, run)
	}
	if !inputPresent {
		return nil, false, fmt.Errorf("loom: child graph %q run %q is parked without resume input", child.Name, run)
	}

	cp, err := loadChildCheckpoint(ctx, store, child.Name, run)
	if err != nil {
		return nil, false, fmt.Errorf("loom: child graph %q run %q checkpoint: %w", child.Name, run, err)
	}
	// Empty YieldPhase is a legacy mid-step checkpoint accepted by Graph.Resume.
	// Keep identity and yielded-state validation here while preserving that
	// reader compatibility for child continuations already stored by ToolLoop.
	if cp.RunID != run || cp.Graph != child.Name || cp.State["__yield"] != true {
		return nil, false, fmt.Errorf("loom: child graph %q run %q continuation does not reference a yielded checkpoint", child.Name, run)
	}
	if err := validateChildCheckpointGeneration(token, seq, cp); err != nil {
		return nil, false, fmt.Errorf("loom: child graph %q run %q stale continuation: %w", child.Name, run, err)
	}
	resumeInput, err := validateChildResumeInput(resumeRaw, cp.State["__toolloop_pending"])
	if err != nil {
		return nil, false, fmt.Errorf("loom: child graph %q run %q resume input: %w", child.Name, run, err)
	}
	var result *loom.RunResult
	if lifecycleHooks == nil {
		result, err = child.Resume(ctx, run, resumeInput, store)
	} else {
		result, err = child.ResumeWithLifecycle(ctx, run, resumeInput, store, *lifecycleHooks)
	}
	if err != nil {
		return result, true, fmt.Errorf("loom: child graph %q run %q resume: %w", child.Name, run, err)
	}
	return result, true, nil
}

func loadChildCheckpoint(ctx context.Context, store loom.Store, graph, runID string) (childCheckpoint, error) {
	data, err := store.Get(ctx, "checkpoint:"+graph, runID)
	if err != nil {
		return childCheckpoint{}, err
	}
	var cp childCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return childCheckpoint{}, err
	}
	return cp, nil
}

func validateChildCheckpointGeneration(token string, seq int64, cp childCheckpoint) error {
	checkpointSeq, valid := protocolInt64(cp.State["__checkpoint_seq"])
	if !valid || seq != cp.Seq || seq != checkpointSeq {
		return fmt.Errorf("checkpoint seq does not match latest child checkpoint")
	}
	checkpointToken, checkpointValid := cp.State["__yield_token"].(string)
	if !checkpointValid || checkpointToken != token {
		return fmt.Errorf("yield token does not match latest child checkpoint")
	}
	return nil
}

func childGraphRevision(child *loom.Graph) string {
	digest := sha256.Sum256([]byte(child.Entry() + "\x00" + strings.Join(child.StepNames(), "\x00")))
	return fmt.Sprintf("%x", digest)
}

func protocolInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		if math.Trunc(v) == v {
			return int64(v), true
		}
	}
	return 0, false
}

func validateChildResumeInput(raw, pendingRaw any) (loom.State, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	var top map[string]json.RawMessage
	if err := decodeStrictJSON(data, &top); err != nil {
		return nil, err
	}
	resultsRaw, ok := top["__resumed_tool_results"]
	if !ok || len(top) != 1 {
		return nil, fmt.Errorf("only __resumed_tool_results is allowed")
	}
	var rawResults map[string]map[string]json.RawMessage
	if err := decodeStrictJSON(resultsRaw, &rawResults); err != nil {
		return nil, fmt.Errorf("decode __resumed_tool_results: %w", err)
	}
	pending, err := decodeToolLoopPending(pendingRaw)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(pending))
	for _, call := range pending {
		allowed[call.CallID] = true
	}
	results := make(map[string]resumedToolResult, len(rawResults))
	for callID, fields := range rawResults {
		if !allowed[callID] {
			return nil, fmt.Errorf("call ID %q is not pending in child checkpoint", callID)
		}
		if len(fields) != 2 || fields["content"] == nil || fields["is_error"] == nil {
			return nil, fmt.Errorf("result %q must contain only content and is_error", callID)
		}
		var result resumedToolResult
		resultData, _ := json.Marshal(fields)
		if err := decodeStrictJSON(resultData, &result); err != nil {
			return nil, fmt.Errorf("decode result %q: %w", callID, err)
		}
		results[callID] = result
	}
	return loom.State{"__resumed_tool_results": results}, nil
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing JSON value")
	}
	return nil
}

func childYieldUpdate(child *loom.Graph, result *loom.RunResult, resumed bool) (loom.State, error) {
	token, tokenOK := result.State["__yield_token"].(string)
	seq, seqOK := protocolInt64(result.State["__checkpoint_seq"])
	if !tokenOK || token == "" || !seqOK || seq <= 0 {
		return nil, fmt.Errorf("loom: child graph %q run %q yielded without checkpoint generation", child.Name, result.RunID)
	}
	update := loom.State{
		"__yield": true, "__yield_phase": "mid_step",
		"__child_run_id": result.RunID, "__child_graph": child.Name,
		"__child_graph_revision": childGraphRevision(child),
		"__child_yield_token":    token, "__child_checkpoint_seq": seq,
	}
	if resumed {
		update["__deleted_keys"] = []string{"__child_resume_input"}
	}
	if result.State["yield_type"] == "await_approval" {
		pending, err := decodeToolLoopPending(result.State["__toolloop_pending"])
		if err != nil {
			return nil, fmt.Errorf("loom: child graph %q run %q pending envelope: %w", child.Name, result.RunID, err)
		}
		envelope := make([]map[string]string, 0, len(pending))
		for _, call := range pending {
			envelope = append(envelope, map[string]string{"park_ref": call.ParkRef, "call_id": call.CallID, "tool": call.Tool})
		}
		update["yield_type"] = "await_approval"
		update["__child_pending"] = envelope
	}
	return update, nil
}

func withChildContinuationCleanup(delta loom.State) loom.State {
	delta["__deleted_keys"] = []string{
		"__child_run_id", "__child_graph", "__child_graph_revision", "__child_yield_token",
		"__child_checkpoint_seq", "__child_resume_input", "__child_pending",
	}
	return delta
}

func subGraphDelta(input, final loom.State, parentCfg *loom.MergeConfig) (loom.State, error) {
	delta := make(loom.State)
	for key, finalValue := range final {
		inputValue, existed := input[key]
		if existed {
			equal, err := equalJSONValue(inputValue, finalValue)
			if err != nil {
				return nil, fmt.Errorf("loom: compare sub-graph state key %q: %w", key, err)
			}
			if equal {
				continue
			}
		} else {
			delta[key] = finalValue
			continue
		}

		candidate, err := mergeAwareDelta(key, inputValue, finalValue, parentCfg)
		if err != nil {
			return nil, err
		}
		delta[key] = candidate
	}
	return delta, nil
}

func mergeAwareDelta(key string, inputValue, finalValue any, parentCfg *loom.MergeConfig) (any, error) {
	nonPrefixSlice := false
	if inputSlice, ok := inputValue.([]any); ok {
		if finalSlice, ok := finalValue.([]any); ok {
			suffix, prefix, err := sliceSuffix(inputSlice, finalSlice)
			if err != nil {
				return nil, fmt.Errorf("loom: compare sub-graph slice key %q: %w", key, err)
			}
			if parentCfg == nil {
				if !prefix {
					return nil, fmt.Errorf("loom: sub-graph slice key %q does not preserve the input prefix", key)
				}
				return suffix, nil
			}
			if prefix {
				matches, err := mergedValueEquals(key, inputValue, suffix, finalValue, parentCfg)
				if err != nil {
					return nil, err
				}
				if matches {
					return suffix, nil
				}
			} else {
				nonPrefixSlice = true
			}
		}
	}

	if parentCfg != nil {
		var difference any
		switch inputNumber := inputValue.(type) {
		case int:
			if finalNumber, ok := finalValue.(int); ok {
				difference = finalNumber - inputNumber
			}
		case float64:
			if finalNumber, ok := finalValue.(float64); ok {
				difference = finalNumber - inputNumber
			}
		}
		if difference != nil {
			matches, err := mergedValueEquals(key, inputValue, difference, finalValue, parentCfg)
			if err != nil {
				return nil, err
			}
			if matches {
				return difference, nil
			}
		}

		matches, err := mergedValueEquals(key, inputValue, finalValue, finalValue, parentCfg)
		if err != nil {
			return nil, err
		}
		if !matches {
			if nonPrefixSlice {
				return nil, fmt.Errorf("loom: sub-graph slice key %q does not preserve the input prefix", key)
			}
			return nil, fmt.Errorf("loom: sub-graph key %q cannot reproduce the child state with the parent merge policy", key)
		}
	}

	return finalValue, nil
}

func sliceSuffix(input, final []any) ([]any, bool, error) {
	if len(input) > len(final) {
		return nil, false, nil
	}
	for i := range input {
		equal, err := equalJSONValue(input[i], final[i])
		if err != nil {
			return nil, false, err
		}
		if !equal {
			return nil, false, nil
		}
	}
	return final[len(input):], true, nil
}

func mergedValueEquals(key string, existing, incoming, want any, cfg *loom.MergeConfig) (bool, error) {
	merged := (loom.State{key: existing}).Merge(loom.State{key: incoming}, cfg)
	return equalJSONValue(merged[key], want)
}

func equalJSONValue(left, right any) (bool, error) {
	leftJSON, err := json.Marshal(left)
	if err != nil {
		return false, err
	}
	rightJSON, err := json.Marshal(right)
	if err != nil {
		return false, err
	}
	return bytes.Equal(leftJSON, rightJSON), nil
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
		initial := func() loom.State {
			msgs, _ := GetMessages(state)
			var rawMsgs []interface{}
			for _, m := range msgs {
				rawMsgs = append(rawMsgs, m)
			}
			handoffState := loom.State{"messages": compressor.Compress(rawMsgs)}
			for _, k := range []string{"session_id", "user_id", "namespace"} {
				if v, ok := state[k]; ok {
					handoffState[k] = v
				}
			}
			return handoffState
		}
		result, resumed, err := runChildContinuation(ctx, state, target, store, nil, initial)
		if err != nil {
			if result != nil {
				return result.State, err
			}
			return nil, err
		}

		// 目标图暂停：走与 YieldBubble 相同的冒泡协议（__child_run_id/__child_graph），
		// 额外记录 handoff_to 标明这是一次移交而非普通子图调用。
		if result.Yielded {
			update, err := childYieldUpdate(target, result, resumed)
			if err != nil {
				return nil, err
			}
			update["handoff_to"] = target.Name
			return update, nil
		}

		// 目标图完成：只回收其最终输出与移交去向，不把目标图的内部状态灌回父图。
		completed := loom.State{
			"output":     result.State["output"],
			"handoff_to": target.Name,
		}
		if resumed {
			return withChildContinuationCleanup(completed), nil
		}
		return completed, nil
	}
}
