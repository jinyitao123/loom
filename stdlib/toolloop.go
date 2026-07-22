package stdlib

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
)

// CompactionPolicy controls automatic context compaction within a ToolLoop.
// CompactionPolicy 定义工具循环内的自动上下文压缩策略，由"触发 / 压缩 / 归档"三件套组成：
// Trigger 决定何时压、Compactor 决定怎么压、PreCompact 在压缩前留档防止原文丢失。
type CompactionPolicy struct {
	// Trigger decides when to compact. Receives messages and estimated token count.
	// 触发器：每次调用 LLM 前，用当前消息列表与粗估 token 数判断是否需要压缩。
	Trigger func(messages []contract.Message, tokenCount int) bool

	// Compactor summarizes old messages into a shorter form.
	// 压缩器：把旧消息摘要成更短的等价形式（通常再调一次 LLM 做总结），返回替换后的消息列表。
	Compactor func(ctx context.Context, messages []contract.Message) ([]contract.Message, error)

	// PreCompact (optional) is called before compaction for archival.
	// 归档钩子（可选）：压缩前拿到完整原始记录用于落盘存档——压缩是有损的，这是保全原文的最后机会。
	PreCompact func(ctx context.Context, transcript []contract.Message) error
}

// StatePatchPolicy opts a ToolLoop into accepting validated control-plane state deltas from tools.
type StatePatchPolicy struct {
	Allowed map[string]map[string]func(value any) error
}

// AllowKeys allows the named tool to replace the listed non-protocol state keys with any JSON value.
func AllowKeys(tool string, keys ...string) *StatePatchPolicy {
	allowed := make(map[string]func(value any) error, len(keys))
	for _, key := range keys {
		allowed[key] = nil
	}
	return &StatePatchPolicy{Allowed: map[string]map[string]func(value any) error{tool: allowed}}
}

// ToolLoopOpts configures the tool loop step.
// ToolLoopOpts 是工具循环步骤的全部可调参数。
type ToolLoopOpts struct {
	Model         string // 使用的模型名，原样透传给 LLM 提供方
	SystemPrompt  string // 静态系统提示词；会被状态键 __system_prompt（若存在且非空）动态覆盖
	MaxIterations int    // 每轮对话允许的最大"LLM→工具"往返次数（即工具调用预算），<=0 时默认 20
	MaxTokens     int    // per-request max output tokens (0 = provider default)
	// MaxTokens：单次请求的最大输出 token 数，0 表示走提供方默认值。
	MaxToolRepeats int // break if same tool call batch repeats N times (0 = default 3)
	// MaxToolRepeats：同一批工具调用连续重复多少次即判定死循环并收束，0 表示默认 3 次。
	OutputSchema *json.RawMessage     // 结构化输出的 JSON Schema，非 nil 时要求模型按此格式作答
	Effort       contract.EffortLevel // v1.4: default EffortMedium
	// Effort：推理强度档位，空值时补齐为中档（EffortMedium）。
	ToolHooks []contract.ToolHook // v1.4: per-tool pre/post hooks
	// ToolHooks：工具级前置/后置钩子——前置可改写或拦截调用，后置用于审计观测。
	Compaction *CompactionPolicy // v1.4: auto-summarize when context grows
	// Compaction：上下文压缩策略，nil 表示不做压缩。
	StatePatchPolicy *StatePatchPolicy
	// StatePatchPolicy：工具状态增量白名单；nil 时任何 StatePatch 都是协议错误。
}

// toolLoopPendingCall is the checkpoint-safe description of one parked tool call.
// toolLoopPendingCall 是单个被暂停工具调用的检查点形态；工具名与参数来自原始 assistant tool_calls。
type toolLoopPendingCall struct {
	CallID  string `json:"call_id"`
	Tool    string `json:"tool"`
	Args    string `json:"args"`
	ParkRef string `json:"park_ref"`
}

// resumedToolResult is the host-supplied real result for a previously parked call.
// resumedToolResult 是宿主在批准或驳回后回灌的真实结果；驳回以 IsError=true 表达。
type resumedToolResult struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
}

// NewToolLoopStep creates the LLM → tool call → result → LLM loop step.
// NewToolLoopStep 构造 agent 的核心循环步骤：问 LLM → 若回复携带工具调用则执行 →
// 把工具结果回填进消息历史 → 再问 LLM，如此往复，直到模型给出纯文本答案、
// 检测到死循环主动收束，或迭代预算耗尽走优雅收尾。
func NewToolLoopStep(llm contract.LLM, tools contract.ToolDispatcher, opts ToolLoopOpts) loom.Step {
	// 迭代预算在闭包外规范化一次即可；opts 是值拷贝，不会影响调用方。
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = 20
	}

	return func(ctx context.Context, state loom.State) (loom.State, error) {
		pending, err := decodeToolLoopPending(state["__toolloop_pending"])
		if err != nil {
			return nil, err
		}
		resumedFromPark := len(pending) > 0
		stagedPatch := make(loom.State)
		stagedPatchJSON := make(map[string][]byte)
		stagedPatchResults := make([]contract.ToolResult, 0)

		var msgs []contract.Message
		if resumedFromPark {
			stagedPatchResults, err = decodeStagedStatePatch(state["__toolloop_staged_patch"])
			if err != nil {
				return nil, err
			}
			if err := stageStatePatches(stagedPatchResults, opts.StatePatchPolicy, stagedPatch, stagedPatchJSON); err != nil {
				return nil, err
			}
			// Resume starts from the private full snapshot, never from "messages": compiler graphs append-merge
			// that public key, which would duplicate the transcript. Checkpoint JSON may have erased Go types.
			// 恢复只读取私有完整快照，绝不读取/写入会被 AppendSlice 合并的 messages；同时兼容 JSON 类型退化。
			msgs, err = decodeToolLoopMessages(state["__toolloop_msgs"])
			if err != nil {
				return nil, err
			}
			resumedResults, err := decodeResumedToolResults(state["__resumed_tool_results"])
			if err != nil {
				return nil, err
			}

			remaining := make([]toolLoopPendingCall, 0, len(pending))
			for _, call := range pending {
				result, ok := resumedResults[call.CallID]
				if !ok {
					remaining = append(remaining, call)
					continue
				}
				toolResult := contract.ToolResult{
					CallID: call.CallID, Content: result.Content,
					IsError: result.IsError, ToolName: call.Tool,
				}
				msgs = append(msgs, toolResult.AsMessage())
			}

			if len(remaining) > 0 {
				return loom.State{
					"__yield":                 true,
					"yield_type":              "await_approval",
					"__toolloop_msgs":         msgs,
					"__toolloop_pending":      remaining,
					"__resumed_tool_results":  nil,
					"__toolloop_staged_patch": stagedPatchResults,
				}, nil
			}

			// Resume invariant: after the assistant tool_calls message, every call_id has exactly one real
			// tool result. Approval and rejection both arrive as ToolResult (rejection uses IsError=true),
			// never as injected system narration.
			// 恢复不变量：assistant tool_calls 之后每个 call_id 恰有一条真实 tool 结果；批准与驳回都以
			// ToolResult 回灌（驳回 IsError=true），绝不注入系统旁白。
			// MaxIterations intentionally restarts here: a resumed run receives a fresh loop budget.
			// MaxIterations 在恢复后从头重计：恢复后的 run 获得一份新的循环预算。
			// StatePatch is intentionally unsupported on resume in this version. If resumed results gain
			// StatePatch later, validate and stage it here using the same path as dispatched results below.
		} else {
			// 从状态取出会话消息历史；取不到属于协议错误，直接失败。
			msgs, err = GetMessages(state)
			if err != nil {
				return nil, err
			}

			// Use __system_prompt from state if available, else fallback to opts.
			// 系统提示词的优先级协议：状态键 __system_prompt（通常由上游提示词组装步骤写入）
			// 覆盖静态配置 opts.SystemPrompt——这让提示词可以逐轮动态生成而无需改动本步骤。
			systemPrompt := opts.SystemPrompt
			if sp, ok := state["__system_prompt"].(string); ok && sp != "" {
				systemPrompt = sp
			}

			// Prepend system prompt if configured.
			// 仅当消息首位还没有 system 消息时才前插，避免与历史中已有的系统消息重复。
			if systemPrompt != "" && (len(msgs) == 0 || msgs[0].Role != "system") {
				msgs = append([]contract.Message{{Role: "system", Content: systemPrompt}}, msgs...)
			}
		}

		// Cache tool list outside loop.
		// 工具清单在循环外只拉取一次并缓存：一轮之内工具集不变，没必要每次迭代都重新列举。
		availableTools, err := tools.ListTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("loom/toolloop: list tools: %w", err)
		}

		// 推理强度缺省补齐为中档。
		effort := opts.Effort
		if effort == "" {
			effort = contract.EffortMedium
		}

		// Cycle detection: track tool call batch signatures.
		// 死循环检测所需状态：对每批工具调用做 SHA-256 签名并与上一批比对，
		// 同一签名连续出现 maxRepeats 次（默认 3）即判定模型在原地打转。
		maxRepeats := opts.MaxToolRepeats
		if maxRepeats <= 0 {
			maxRepeats = 3
		}
		var lastBatchHash string // 上一批工具调用的签名哈希
		repeatCount := 0         // 当前签名连续重复的次数

		// 主循环：最多 MaxIterations 次"LLM→工具"往返。
		for i := 0; i < opts.MaxIterations; i++ {
			// Context compaction check.
			// 每次调 LLM 前先做上下文压缩检查：触发则先归档（若配置）再压缩，
			// 用摘要后的消息列表替换原历史，防止长会话撑爆模型上下文窗口。
			if opts.Compaction != nil {
				tokenCount := estimateMessagesTokens(msgs) // 粗估当前历史的 token 总量
				if opts.Compaction.Trigger(msgs, tokenCount) {
					// 归档是尽力而为：钩子失败不阻塞压缩（返回值被有意忽略）。
					if opts.Compaction.PreCompact != nil {
						opts.Compaction.PreCompact(ctx, msgs)
					}
					// 压缩失败则整轮失败——带着坏历史继续跑只会越错越远。
					msgs, err = opts.Compaction.Compactor(ctx, msgs)
					if err != nil {
						return nil, fmt.Errorf("loom/toolloop: compaction failed: %w", err)
					}
				}
			}

			// 本轮询问 LLM：携带完整历史、工具清单与输出约束。
			resp, err := llm.Chat(ctx, contract.ChatRequest{
				Model:     opts.Model,
				Messages:  msgs,
				Tools:     availableTools,
				MaxTokens: opts.MaxTokens,
				Schema:    opts.OutputSchema,
				Effort:    effort,
			})
			// 调用失败：把错误文本写入状态键 __error（供下游路由器/观测使用）并同时向引擎上抛。
			if err != nil {
				return loom.State{"__error": err.Error()}, err
			}

			// 模型没有再请求工具 ⇒ 任务在本轮完成，把文本答案与用量写入输出状态，正常收官。
			if len(resp.ToolCalls) == 0 {
				return finishToolLoop(resp.Content, resp.Usage, resumedFromPark, stagedPatch), nil
			}

			// Cycle detection: hash current tool call batch and compare to previous.
			// 死循环检测：对本批工具调用（名称+参数）求 SHA-256 签名，与上一批比对。
			batchHash := hashToolCalls(resp.ToolCalls)
			if batchHash == lastBatchHash {
				repeatCount++ // 与上一批完全相同：累计重复次数
				// 连续重复达到阈值：判定死循环，主动收束——直接把当前回复内容作为输出返回，
				// 而不是无谓地烧完剩余迭代预算。
				if repeatCount >= maxRepeats {
					slog.Warn("loom/toolloop: cycle detected, breaking",
						"repeat_count", repeatCount, "batch_hash", batchHash[:8]) // 哈希只记前 8 位，够日志检索用
					return finishToolLoop(resp.Content, resp.Usage, resumedFromPark, nil), nil
				}
			} else {
				// 出现新的调用组合：更新基准签名，计数从 1 重新累计。
				lastBatchHash = batchHash
				repeatCount = 1
			}

			// Dispatch with hooks and read/write awareness.
			// 执行本批工具调用（带钩子、按只读/有状态分流，见 DispatchWithHooks）。
			results, err := DispatchWithHooks(ctx, tools, resp.ToolCalls, availableTools, opts.ToolHooks)
			if err != nil {
				return nil, err
			}
			if err := stageStatePatches(results, opts.StatePatchPolicy, stagedPatch, stagedPatchJSON); err != nil {
				return nil, err
			}
			for _, result := range results {
				if result.StatePatch != nil {
					stagedPatchResults = append(stagedPatchResults, result)
				}
			}
			if hasParkedToolResult(results) {
				// The assistant tool_calls message is checkpointed once. Executed siblings keep their real
				// results; parked placeholders are omitted and represented only by the pending protocol.
				// assistant 工具调用消息只入快照一次；同批已执行结果照常入史，park 占位文本绝不入史。
				msgs = append(msgs, resp.AsMessage())
				callsByID := make(map[string]contract.ToolCall, len(resp.ToolCalls))
				for _, call := range resp.ToolCalls {
					callsByID[call.ID] = call
				}
				parked := make([]toolLoopPendingCall, 0, len(results))
				for _, result := range results {
					if !result.Park {
						msgs = append(msgs, result.AsMessage())
						continue
					}
					call := callsByID[result.CallID]
					parked = append(parked, toolLoopPendingCall{
						CallID: result.CallID, Tool: call.Name, Args: call.Args, ParkRef: result.ParkRef,
					})
				}
				return loom.State{
					"__yield":                 true,
					"yield_type":              "await_approval",
					"__toolloop_msgs":         msgs,
					"__toolloop_pending":      parked,
					"__toolloop_staged_patch": stagedPatchResults,
				}, nil
			}
			// 结果回填协议：先追加 assistant 的工具调用消息，再逐条追加对应的工具结果消息，
			// 保持"调用在前、结果在后"的顺序供下一轮 LLM 阅读。
			msgs = append(msgs, resp.AsMessage())
			for _, r := range results {
				msgs = append(msgs, r.AsMessage())
			}
		}

		// Tool budget exhausted. Don't fail the whole turn (which surfaces to the
		// end user as a raw error) — inject a wrap-up note and make one final call
		// with NO tools offered, so the model must close out in text: summarize
		// what it has, and say how the remainder should be handled.
		// 迭代预算耗尽的优雅收尾：不抛错误——裸错误会一路穿透直接砸到最终用户脸上；
		// 而是注入一条"收尾指令"，再做最后一次【不提供任何工具】的调用，
		// 模型无工具可调，只能以文字总结已有发现、说明未竟事项与后续处理建议。
		slog.Warn("loom/toolloop: tool budget exhausted, wrapping up",
			"max_iterations", opts.MaxIterations)
		// 收尾指令以 user 角色注入：告知预算已尽、禁止再请求工具、要求用会话语言做总结。
		msgs = append(msgs, contract.Message{
			Role: "user",
			Content: fmt.Sprintf("[loom] The tool-call budget for this turn is exhausted (%d rounds used). "+
				"Do not request any more tool calls. In the language of this conversation: summarize what you "+
				"have found so far, state clearly what remains unfinished, and suggest how the remaining work "+
				"should be handled (for example, delegating it as a background task).", opts.MaxIterations),
		})
		// 最后一问：请求里刻意不带 Tools 字段——从协议层面断掉继续调用工具的可能性。
		resp, err := llm.Chat(ctx, contract.ChatRequest{
			Model:     opts.Model,
			Messages:  msgs,
			MaxTokens: opts.MaxTokens,
			Effort:    effort,
		})
		// 收尾调用也失败时，同样记录 __error 并上抛。
		if err != nil {
			return loom.State{"__error": err.Error()}, err
		}
		// 把收尾总结作为本轮的最终输出返回。
		return finishToolLoop(resp.Content, resp.Usage, resumedFromPark, nil), nil
	}
}

// decodeToolLoopMessages restores a private message snapshot after checkpoint JSON round-tripping.
// decodeToolLoopMessages 将检查点中退化为 []any/map[string]any 的私有消息快照还原为强类型消息。
func decodeToolLoopMessages(raw any) ([]contract.Message, error) {
	if messages, ok := raw.([]contract.Message); ok {
		return messages, nil
	}
	if raw == nil {
		return nil, fmt.Errorf("loom/toolloop: missing __toolloop_msgs for pending tool calls")
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("loom/toolloop: encode __toolloop_msgs: %w", err)
	}
	var messages []contract.Message
	if err := json.Unmarshal(data, &messages); err != nil {
		return nil, fmt.Errorf("loom/toolloop: decode __toolloop_msgs: %w", err)
	}
	return messages, nil
}

// decodeToolLoopPending accepts both in-memory structs and checkpoint-decoded JSON values.
// decodeToolLoopPending 同时兼容内存强类型切片与检查点恢复后的 []any/map[string]any。
func decodeToolLoopPending(raw any) ([]toolLoopPendingCall, error) {
	if raw == nil {
		return nil, nil
	}
	if pending, ok := raw.([]toolLoopPendingCall); ok {
		return pending, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("loom/toolloop: encode __toolloop_pending: %w", err)
	}
	var pending []toolLoopPendingCall
	if err := json.Unmarshal(data, &pending); err != nil {
		return nil, fmt.Errorf("loom/toolloop: decode __toolloop_pending: %w", err)
	}
	return pending, nil
}

// decodeResumedToolResults accepts the host's map[call_id]{content,is_error} in typed or JSON form.
// decodeResumedToolResults 宽容解码宿主回灌结果，兼容 map[string]any 及检查点 JSON 形态。
func decodeResumedToolResults(raw any) (map[string]resumedToolResult, error) {
	if raw == nil {
		return nil, nil
	}
	if results, ok := raw.(map[string]resumedToolResult); ok {
		return results, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("loom/toolloop: encode __resumed_tool_results: %w", err)
	}
	var results map[string]resumedToolResult
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, fmt.Errorf("loom/toolloop: decode __resumed_tool_results: %w", err)
	}
	return results, nil
}

func decodeStagedStatePatch(raw any) ([]contract.ToolResult, error) {
	if raw == nil {
		return nil, nil
	}
	if results, ok := raw.([]contract.ToolResult); ok {
		return results, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("loom/toolloop: encode __toolloop_staged_patch: %w", err)
	}
	var results []contract.ToolResult
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, fmt.Errorf("loom/toolloop: decode __toolloop_staged_patch: %w", err)
	}
	return results, nil
}

// hasParkedToolResult reports whether a dispatch batch requests a run-level approval yield.
// hasParkedToolResult 判断本批结果是否包含要求整轮暂停等待批准的 park 标记。
func hasParkedToolResult(results []contract.ToolResult) bool {
	for _, result := range results {
		if result.Park {
			return true
		}
	}
	return false
}

// finishToolLoop clears park protocol state only for a resumed run, preserving the no-park delta exactly.
// finishToolLoop 仅在 park 恢复后的正常出口清场，保证从未 park 的路径仍返回原有增量。
func finishToolLoop(content string, usage contract.Usage, resumedFromPark bool, stagedPatch loom.State) loom.State {
	result := SetOutput(content, usage)
	for key, value := range stagedPatch {
		result[key] = value
	}
	if resumedFromPark {
		result["__toolloop_pending"] = nil
		result["__toolloop_msgs"] = nil
		result["__resumed_tool_results"] = nil
		result["__toolloop_staged_patch"] = nil
	}
	return result
}

// stageStatePatches validates post-hook tool results and accumulates patches until normal ToolLoop completion.
func stageStatePatches(results []contract.ToolResult, policy *StatePatchPolicy, staged loom.State, stagedJSON map[string][]byte) error {
	for _, result := range results {
		if result.StatePatch == nil {
			continue
		}
		if result.IsError {
			return fmt.Errorf("loom/toolloop: StatePatch protocol violation: IsError result %q carries a patch", result.CallID)
		}
		if result.Park {
			return fmt.Errorf("loom/toolloop: StatePatch protocol violation: Park result %q carries a patch", result.CallID)
		}
		if policy == nil {
			return fmt.Errorf("loom/toolloop: StatePatch policy is required for result %q", result.CallID)
		}
		if result.ToolName == "" {
			return fmt.Errorf("loom/toolloop: StatePatch tool name is empty for result %q", result.CallID)
		}
		allowedKeys, allowedTool := policy.Allowed[result.ToolName]
		if !allowedTool {
			return fmt.Errorf("loom/toolloop: StatePatch tool %q is not allowed", result.ToolName)
		}
		for key, value := range result.StatePatch {
			if isReservedStatePatchKey(key) {
				return fmt.Errorf("loom/toolloop: StatePatch key %q is reserved", key)
			}
			validate, allowed := allowedKeys[key]
			if !allowed {
				return fmt.Errorf("loom/toolloop: StatePatch key %q is not allowed for tool %q", key, result.ToolName)
			}
			if validate == nil && strings.HasPrefix(key, "__") {
				return fmt.Errorf("loom/toolloop: StatePatch key %q requires an explicit validator", key)
			}
			if validate != nil {
				if err := validate(value); err != nil {
					return fmt.Errorf("loom/toolloop: StatePatch key %q validation failed: %w", key, err)
				}
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				return fmt.Errorf("loom/toolloop: StatePatch key %q value is not JSON serializable: %w", key, err)
			}
			if previous, exists := stagedJSON[key]; exists {
				if string(previous) != string(encoded) {
					return fmt.Errorf("loom/toolloop: StatePatch conflict for key %q", key)
				}
				continue
			}
			staged[key] = value
			stagedJSON[key] = encoded
		}
	}
	return nil
}

func isReservedStatePatchKey(key string) bool {
	return key == "output" || key == "usage" || strings.HasPrefix(key, "__toolloop_") ||
		strings.HasPrefix(key, "__yield") || key == "__resumed_tool_results"
}

// DispatchWithHooks runs tool hooks, then dispatches with read/write awareness.
// DispatchWithHooks 按三阶段协议执行一批工具调用：
//
//	阶段 1：Pre 钩子——可改写调用参数，也可返回错误直接拦截（结果标记为被钩子挡下）；
//	阶段 2：按 ToolDef.ReadOnly 分流执行——只读工具并行跑（无副作用、互不干扰），
//	        有状态工具串行跑（副作用之间可能有依赖，必须保持模型给出的顺序）；
//	阶段 3：Post 钩子——拿到调用与结果指针做审计/改写。
//
// 返回的结果切片与传入的 calls 按下标一一对应，保证回填消息历史时顺序正确。
func DispatchWithHooks(ctx context.Context, tools contract.ToolDispatcher,
	calls []contract.ToolCall, defs []contract.ToolDef, hooks []contract.ToolHook) ([]contract.ToolResult, error) {

	// Build def lookup map.
	// 建立"工具名 → 定义"查找表，供阶段 2 读取 ReadOnly 标记做分流。
	defMap := make(map[string]contract.ToolDef)
	for _, d := range defs {
		defMap[d.Name] = d
	}

	// 结果槽位与调用一一对应：先占位，后续各阶段按下标填充。
	results := make([]contract.ToolResult, len(calls))

	// 阶段 1：对每个调用依次执行 Pre 钩子链。
	for i, call := range calls {
		// Phase 1: run Pre hooks (can modify call or block).
		blocked := false // 本调用是否已被某个钩子拦截
		for _, h := range hooks {
			// 钩子带 Matcher 且不匹配该工具名时，此钩子对本调用不生效。
			if h.Matcher != nil && !h.Matcher(call.Name) {
				continue
			}
			if h.Pre != nil {
				// Pre 钩子返回改写后的调用；返回错误则视为拦截本次调用。
				modified, err := h.Pre(ctx, call)
				if err != nil {
					// 拦截不等于崩溃：生成一条 IsError 结果告知模型"被钩子挡下"，循环得以继续。
					results[i] = contract.ToolResult{
						CallID:   call.ID,
						Content:  fmt.Sprintf("blocked by hook: %v", err),
						IsError:  true,
						ToolName: call.Name,
					}
					blocked = true
					break // 已拦截，后续钩子不再执行
				}
				// 采纳钩子的改写结果，串给链上的下一个钩子（钩子按序链式生效）。
				call = modified
			}
		}

		// 被拦截的调用不回写、不执行，直接处理下一个。
		if blocked {
			continue
		}

		// 把（可能被改写的）调用写回原切片，供阶段 2 执行、阶段 3 的 Post 钩子引用。
		calls[i] = call // store potentially modified call
	}

	// Phase 2: split by read-only vs stateful.
	// 阶段 2 准备：给调用附带原始下标一起分流，执行完仍能把结果写回正确槽位。
	type indexedCall struct {
		idx  int               // 在 calls/results 中的原始下标
		call contract.ToolCall // 待执行的调用（阶段 1 改写后的版本）
	}
	var readOnly, stateful []indexedCall
	for i, call := range calls {
		// CallID 非空说明该槽位已在阶段 1 被填过（拦截结果），跳过执行。
		if results[i].CallID != "" {
			continue // already blocked
		}
		// 依据工具定义的 ReadOnly 标记分流；查不到定义的工具一律按有状态保守处理。
		if d, ok := defMap[call.Name]; ok && d.ReadOnly {
			readOnly = append(readOnly, indexedCall{i, call})
		} else {
			stateful = append(stateful, indexedCall{i, call})
		}
	}

	// Read-only: parallel.
	// 只读工具无副作用，可安全并行执行；用 WaitGroup 等待全部 goroutine 结束。
	if len(readOnly) > 0 {
		var wg sync.WaitGroup
		for _, ic := range readOnly {
			wg.Add(1)
			// 下标与调用通过参数传入 goroutine，规避闭包捕获循环变量的经典陷阱；
			// 各 goroutine 写入 results 的不同下标，无数据竞争。
			go func(idx int, c contract.ToolCall) {
				defer wg.Done()
				result, err := tools.Dispatch(ctx, c)
				if err != nil {
					// 单个工具失败不连坐：转成 IsError 结果，让模型自行决定如何应对。
					results[idx] = contract.ToolResult{
						CallID: c.ID, Content: fmt.Sprintf("error: %v", err),
						IsError: true, ToolName: c.Name,
					}
					return
				}
				// 补上工具名（分发器不保证回填），再落入对应槽位。
				result.ToolName = c.Name
				results[idx] = *result
			}(ic.idx, ic.call)
		}
		wg.Wait() // 汇合点：所有只读调用完成后才进入串行阶段
	}

	// Stateful: serial (order matters).
	// 有状态工具严格串行、保持模型给出的顺序——写操作之间可能存在先后依赖。
	for _, ic := range stateful {
		result, err := tools.Dispatch(ctx, ic.call)
		if err != nil {
			// 失败同样转成 IsError 结果并继续执行后续调用，不中断整批。
			results[ic.idx] = contract.ToolResult{
				CallID: ic.call.ID, Content: fmt.Sprintf("error: %v", err),
				IsError: true, ToolName: ic.call.Name,
			}
			continue
		}
		result.ToolName = ic.call.Name // 同样补上工具名
		results[ic.idx] = *result
	}

	// Phase 3: run Post hooks.
	// 阶段 3：对每条结果执行 Post 钩子（同样受 Matcher 过滤）。
	// 钩子拿到原调用和结果指针，可用于审计留痕、脱敏或原地改写结果内容。
	for i := range results {
		for _, h := range hooks {
			// Match against the actual post-Pre dispatch name, not mutable result data.
			if h.Matcher != nil && !h.Matcher(calls[i].Name) {
				continue
			}
			if h.Post != nil {
				if err := h.Post(ctx, calls[i], &results[i]); err != nil {
					return nil, fmt.Errorf("loom/toolloop: post hook for tool %q: %w", calls[i].Name, err)
				}
			}
		}
		// ToolName is provenance, not hook-controlled result content.
		results[i].ToolName = calls[i].Name
	}

	// 返回与 calls 等长、按下标对齐的结果集。
	return results, nil
}

// hashToolCalls creates a deterministic hash of a tool call batch for cycle detection.
// hashToolCalls 对一批工具调用生成确定性 SHA-256 签名，作为死循环检测的比对依据：
// 按顺序把每个调用的名称与原始参数写入哈希——名称、参数、顺序完全一致的两批调用签名相同。
func hashToolCalls(calls []contract.ToolCall) string {
	h := sha256.New()
	for _, c := range calls {
		h.Write([]byte(c.Name)) // 工具名参与签名
		h.Write([]byte(c.Args)) // 原始参数（JSON 字节）参与签名——参数不同就不算重复
	}
	return hex.EncodeToString(h.Sum(nil)) // 十六进制编码，便于日志展示与字符串比较
}

// estimateMessagesTokens provides a rough token count (~4 chars per token).
// estimateMessagesTokens 用"约 4 字符 ≈ 1 token"的经验值粗估整段历史的 token 量，
// 只为上下文压缩的触发判断提供量级参考，不追求与真实分词器一致。
func estimateMessagesTokens(msgs []contract.Message) int {
	total := 0
	for _, m := range msgs {
		// 正文按 4 字符/token 折算，另加每条消息约 4 个 token 的结构开销（角色、分隔符等）。
		total += len(m.Content)/4 + 4 // content + overhead per message
	}
	return total
}
