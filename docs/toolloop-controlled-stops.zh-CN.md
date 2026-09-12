# ToolLoop 受控停止与持久片段设计 v0.2

状态：推荐方案已采纳。Loom Layer 2 的可选控制路径、读取和纯转换接口已经实现，正在独立 worktree 中审阅；本文件中的 Weave 联合接续仍是待实施合同，不代表已经接通。

源码基准为 Loom `v0.8.1` 加本地提交 `60ef01352c45782f1246dcfc4ccf22768a5c9a72`。该提交仅修图级预算错误包装和拒绝步骤的恢复位置。联合核对的 Weave HEAD 为 `ce5fe66701083237398df038998899aa6d57fe7c`，其依赖仍为 Loom `v0.8.1`。

修订记录（2026-09-08）：v0.1 的配套用法文档曾声称 “NewSubGraphStep and Handoff reject controlled child loops before effects”。v0.2 撤回这个全图保证，明确保护仅覆盖受控 ToolLoop 自身的 model/tool 调用；此前子图步骤的效果仍会发生。全拓扑预检归属已知 frozen 配置的宿主发布/组合层，本批不扩展 Layer 0，也不增加函数指针注册表。新增真实 legacy ToolLoop 前置步骤的回归，分别验证 SubGraph 与 Handoff 的这个边界。

## 1. 建议与范围

建议首批复用现有 `Step → delta → Graph merge → checkpoint → yield` 路径。ToolLoop 在受控停止时返回含 `__yield: true`、`__yield_phase: "mid_step"` 的有效增量及 nil error；Graph 返回 `StopYielded`。退出的具体原因通过 stdlib 的版本化记录和类型化读取接口传递。普通步骤错误的现有语义保持不变。任何需要保存的受控停止现场，都不得通过普通 `delta + error` 出口传递。

这条路径不需要增加 Layer 0 API。初次启用限定在单个串行 leaf graph 的 ToolLoop；持久运行必须提供 Store 并选择 `CheckpointRequired`。Weave 负责预算授权、进展判断、副作用核对、journal、attempt fencing 和产品状态。Loom 不增加任务调度、用户审批或业务验收原语。

两个恢复保证分别验证：

- Loom 的私有快照保证已保存暂停边界的继续执行。
- Weave 的 journal 保证片段内崩溃后从入口重放已确认 model/tool 回执。普通嵌入者只有 step checkpoint 时，不能据此获得片段内工具效果不重复的保证。

本批不为嵌套子图、跨 run 的 ToolLoop fork、CLI wire events 或 Weave 前台业务执行增加能力。已有这些路径保持默认配置；启用新模式前，宿主发布/组合检查必须拒绝尚未具备对应适配的拓扑。该预检是待实施的 Weave 接点，不能由 Loom 的局部运行时拒绝替代。

## 2. 可实施 API

下列声明对应本批 stdlib 接口，不修改现有 `Step`、`Graph`、`Store` 或 `contract.LLM` 签名。

```go
type ToolLoopControl struct {
    ID                 string // 编译时固定，如 "chat"
    InitialTotalRounds uint64 // 首次授权的总轮数，必须大于 0
}

// 在既有 ToolLoopOpts 中增加字段：
Control *ToolLoopControl // nil 完全保留 v0.8.1 的行为

type ToolLoopStopReason string

const (
    ToolLoopFinalResponse ToolLoopStopReason = "final_response"
    ToolLoopToolStop      ToolLoopStopReason = "tool_stop"
    ToolLoopSliceLimit    ToolLoopStopReason = "slice_limit"
    ToolLoopTotalLimit    ToolLoopStopReason = "total_limit"
    ToolLoopRepeatLimit   ToolLoopStopReason = "repeat_limit"
    ToolLoopProviderLength ToolLoopStopReason = "provider_length"
    ToolLoopProviderStop   ToolLoopStopReason = "provider_stop"
    ToolLoopAwaitToolResult ToolLoopStopReason = "await_tool_result"
)

type ToolLoopOutcome struct {
    Version            uint32
    RunID, LoopID      string
    Reason             ToolLoopStopReason
    Slice              uint64
    SliceRoundsUsed    uint64
    TotalRoundsUsed    uint64
    AuthorizedTotalRounds uint64
    ProviderStopReason string
    PartialText        string
}

type ToolLoopResumeGrant struct {
    ID                    string
    ExpectedRunID         string
    ExpectedCheckpointSeq int64
    ExpectedYieldToken    string
    ExpectedSlice         uint64
    AuthorizedTotalRounds uint64 // 绝对上限，避免重复请求重复相加
}

func ReadToolLoopOutcome(state loom.State) (ToolLoopOutcome, bool, error)
func PrepareToolLoopResume(state loom.State, grant ToolLoopResumeGrant) (loom.State, error)
```

`ReadToolLoopOutcome` 对缺失的新协议返回 `present=false`；对损坏或身份不符的新协议返回 error，不能退回 legacy 路径或补零。有效 active 状态返回 `present=true` 且 reason 为空。返回值用于判断机制结果，不能证明保存已经成功或交付验收通过；调用方必须先检查 Graph 返回错误与实际持久回执。

`PrepareToolLoopResume` 是纯函数，返回交给 `Graph.Resume` 的增量。它不调用网络、写存储、判定用户授权或启动执行。只允许 `slice_limit`、`total_limit` 且没有 pending tool calls 的暂停状态进入新片段；检查 run ID、checkpoint seq、yield token、slice 和授权 ID。授权上限不得减少，且必须大于累计已用轮数。

这里的 checkpoint seq 取已保存快照的 `__seq`；额度暂停的 `__checkpoint_seq` 必须与之相同，yield token 取 `__yield_token`。函数返回的增量只包含更新后的私有控制记录及保持原值的私有快照，不改写 Graph 身份、checkpoint seq 或旧 yield token。Graph.Resume 在重入时消费旧 yield 键。宿主通过控制记录中的 `last_grant_id` 查找完整持久授权，并在新片段入口事务再次核验原边界，不能把纯函数校验视为并发裁决。

同一授权 ID 的重复请求由宿主的持久授权回执返回之前的结果，不再次调用该转换函数。重复 ID 改载荷、旧 checkpoint、旧 token、已应用到后续片段的授权均不能重新置零片段计数。stdlib 转换函数对这些不满足前置条件的状态拒绝执行。

纯转换只核验传入的快照，无法知道 Store 中是否已经出现更新的快照。对旧快照重复调用转换函数仍会得到同一个有效增量；直接向 Graph.Resume 重放旧完整增量也可能覆盖最新状态。这些跨请求竞争和去重必须由宿主的持久授权回执与 source checkpoint CAS 阻止。裸 Graph.Resume 不能替代宿主 CAS，本批不向 Graph 增加授权语义。

首次运行需要 Graph 注入的非空 `__run_id`，并绑定 `Control.ID`。控制模式的同一逻辑循环只在同一 run 中恢复。携带旧控制记录、却改变 run ID 的输入不能自动获得新额度。跨 run fork 的授权规则另行实现。

## 3. 配置与计数口径

`Control == nil` 时保留旧输出、旧 park 行为、默认 20 轮以及耗尽后的无工具总结调用。现有默认路径测试不改成新语义。

启用控制模式后：

| 配置 | 含义 |
|---|---|
| 既有 `MaxIterations` | 每个执行片段的主模型调用上限；`<=0` 仍规范化为 20 |
| `Control.InitialTotalRounds` | 初始累计轮数上限；不能用 0 表示无限或未知 |
| 既有 `MaxToolRepeats` | 连续相同工具批次阈值；`<=0` 仍为 3 |
| 既有 `MaxTokens` | 单次输出上限；不等于总 token 额度 |
| Weave frozen limits 与 usage receipts | 累计 token、费用及未知消耗的权威账本 |

一轮是一次主 `llm.Chat` 的逻辑调用，不是一件工具，也不是恢复尝试。最终无工具回答、被重复检测拦住的响应、provider 长度截断响应都占一轮；一批多个工具仍占一轮。模型回包丢失后的重试保留同一逻辑调用，实际 physical attempts 的消耗由宿主账本分别记录，不能称为零消耗。

主调用前检查剩余总额度和片段额度；响应拿到后增加已用逻辑轮数，再处理停止原因和工具批次。模型返回错误时交给宿主原有回执与重试策略，不凭该错误创建新片段。压缩模型调用和可能保留的展示总结调用都必须进入宿主用量账本；它们不冒充主循环轮数。本批控制模式在预算出口不追加总结模型调用。

默认例子是每片段 20 轮、总额 60 轮。第 20 轮后保存 `slice_limit`；宿主核实进展和副作用后可用新授权 ID 继续下一片段，总额仍为 60。第 60 轮后保存 `total_limit`；只有明确记录更高的绝对总上限才能继续。Graph 的 `WithStepBudget` 是另一层图步骤额度，继续保留其原有累计规则。

## 4. 私有持久状态

新增 `__toolloop_control`，复用现有私有消息、park、staged patch 和 usage 字段。不得从 `pending` 数量推断有没有快照；新记录显式定义快照是否有效。新模式下所有协议字段组成一个不可拆分的快照。

| 字段 | 内容与不变量 |
|---|---|
| `version` | 固定为 1；未知版本执行前拒绝 |
| `run_id` / `loop_id` | 对应当前 Graph run 和编译时控制 ID |
| `policy_sha256` | 规范化后的模型名、有效 system prompt、输出 schema/上限、effort、片段上限、初始总额、重复阈值与控制协议版本摘要；恢复必须匹配 |
| `phase` | `active`、`paused`、`complete` |
| `slice` | 从 1 开始，只有已核验的新片段授权能增加 |
| `slice_limit` | 规范化后的 `MaxIterations`，用于独立检查停止原因与计数 |
| `slice_start_round` | 本片段开始时已经消费的总轮数 |
| `slice_rounds_used` | 本片段已确认的主模型逻辑轮数 |
| `total_rounds_used` | 必须等于 `slice_start_round + slice_rounds_used`；累计不回退 |
| `authorized_total_rounds` | 初始额度或最近一次已保存授权的绝对上限 |
| `last_batch_hash` / `repeat_count` | 沿用当前按名称、原始参数和顺序计算的签名；跨片段与 park 延续 |
| `last_grant_id` | 最近一次新片段授权 ID；用于绑定宿主回执 |
| `reason` / `provider_stop_reason` / `partial_text` | 暂停或完成原因；异常 provider 暂停保留原始停止原因和可选 partial text |
| `halted_response` | 长度截断或重复拦截时尚未执行工具的原始响应；不伪装成已完成工具历史 |
| `__toolloop_msgs` | 完整私有消息快照，不追加合并到公共 `messages` |
| `__toolloop_pending` | 尚未收到真实结果的 parked calls；空集合也必须显式保存 |
| `__toolloop_staged_patch` | 已验证但尚未提交的工具 StatePatch/StateOps 及顺序 |
| `__toolloop_usage` | 本循环已报告用量的汇总；不替代宿主 physical-attempt 账本 |

解码必须兼容经过 JSON 的 Go 类型退化，同时拒绝非整数、负数、溢出、缺字段及相互矛盾的计数。计数采用 `0..2^53-1` 的可精确 JSON 往返整数范围，增加前检查上界。只恢复同一 policy；不得通过重建闭包、修改 `MaxIterations` 或清空私有键隐式重置总额。

policy 摘要不散列 Go 函数地址，也不能证明 hook、compactor、工具实现或动态工具目录等价；这些实现身份继续由宿主的 frozen 发布身份绑定。新控制记录只补原 ToolLoop 局部变量未持久化的轮数、重复签名与暂停身份，复用现有消息、usage、pending 和 staged patch 字段。图级 StepBudget 计算 Graph 步骤次数，无法替代一个步骤内部的模型轮数。

循环入口深拷贝这些字段，再在局部状态上构建后续进度。不得原地修改 Graph 提供的入口消息切片或控制记录，因为 Weave 的片段内重放依赖入口快照不变。提供给 compactor 的消息也需遵守这一约束。

受控暂停的增量保留全部私有快照，设置 `__yield=true`、`__yield_phase="mid_step"`、`yield_type="toolloop_control"`，不调用 `SetOutput`，也不提交公共 staged patch。真实循环结束时沿用 `SetOutput` 写公共 `output` 和 `usage`，最终 assistant 消息留在私有消息快照，公共 `messages` 不追加整个循环历史。完成时记录 `phase=complete` 和 `__yield_phase="after_step"`，避免后续普通 Resume 重新执行已经完成的 ToolLoop。

## 5. 停止结果与安全点

| 触发 | 图结果 | ToolLoop 结果 | 后续行为 |
|---|---|---|---|
| 普通无工具回答 | `StopCompleted`，nil error | `final_response` | 只表示循环结束；交付另行核验 |
| 有效 `ToolResult.StopLoop` | 现有正常路由／完成 | `tool_stop` | 按原契约提交 staged patch |
| 片段上限，仍有总额 | `StopYielded`，nil error | `slice_limit` | 保存后由宿主判断是否授权下一片段 |
| 累计总额耗尽 | `StopYielded`，nil error | `total_limit` | 没有新增总授权时执行数保持不变 |
| 同一工具批次达到重复阈值 | `StopYielded`，nil error | `repeat_limit` | 本批未执行；首批设计不自动接续 |
| provider 返回 `length` | `StopYielded`，nil error | `provider_length` | 在执行该响应中的工具前停止；首批不自动接续 |
| provider 明确给出其他非正常停止 | `StopYielded`，nil error | `provider_stop` | 保留原原因，交宿主处理 |
| 工具 park | 现有 `StopYielded` | `await_tool_result` | 等真实结果，同一片段计数继续 |
| 网络、协议、工具效果未知或存储错误 | 原有错误分类 | 不生成成功或新片段授权 | 由宿主原有恢复／核对处理 |

这里的 `StopYielded` 是 Graph 让出执行的机制事实，`ToolLoopOutcome.Reason` 是具体退出原因。ToolLoop 的轮数暂停不借用图级 `ErrBudgetExhausted`，调用方不得靠匹配错误或总结文字来识别它。

在新模式中，provider `length` 优先于“没有 tool calls”以及工具派发。重复检测继续在派发之前完成。被截断或被重复拦截的 assistant tool calls 保存在 `halted_response`，不写成待执行队列，不补造 ToolResult，也不把带悬空 call ID 的消息交给下一轮模型。

首批识别 provider 已透传的 `stop`、`tool_calls`、`length`，空值沿用既有结构判断以兼容没有填写停止原因的适配器。`stop` 携带 tool calls 或 `tool_calls` 没有 calls 属于响应协议冲突，执行工具前报错；其他非空值保存 `provider_stop`，不得猜成最终回答。新的 provider 停止原因映射需要显式扩展并测试。

预算安全点位于上一主调用的全部工具结果已经收齐并加入私有历史之后、下一主调用开始之前。到达同一边界时总额耗尽优先于片段耗尽。`repeat_count` 在新片段授权时不清零，避免每次接续都绕过重复保护。

park 恢复沿用已有真实结果协议。主调用在 park 前已消费的轮数必须保留；只有全部 pending 结果补齐后才能检查下一轮额度，审批恢复本身不给新片段。如果片段恰在 park 前用完，补齐结果后立即形成额度暂停，不额外调用模型。Weave 首批仍保留其当前“不支持 interactive tool yield”的入口限制。

直接对其他暂停状态调用 Graph.Resume 而没有有效额度转换，也不能执行新 model/tool operation；ToolLoop 保持原暂停原因并再次让出。宿主正常轮询直接读取暂停回执，不用这种重新保存检查点的方式轮询。

## 6. 完成钩子与保存顺序

现有 Graph 顺序为 merge、After hooks、checkpoint、yield。因此复用 yield 需要宿主明确区分安全检查与完成副作用。

Weave frozen compiler 在完整组装 After hooks 后统一包装：当有效新协议处于 `phase=paused` 时，跳过 AutoRemember、旧闭包式预算累计及其他只应在完成时运行的 hooks。`memberAfterStep` 保留 fatal/outcome-unknown 检查；合法额度暂停不标记 `__member_step_complete=true`，不改成 `after_step`，也不作为审批 yield 拒绝。非法私有状态仍终止执行，不能绕过安全检查。

成功的额度停止顺序固定为：

1. ToolLoop 生成有效私有增量，返回 nil error。
2. Graph 合并一次；宿主检查 fatal 状态并跳过普通完成副作用。
3. Store 以 Required 策略保存新的完整暂停检查点；Weave 同时保存暂停边界回执。
4. Graph 返回 `StopYielded`，宿主验证 `outcome` 与已保存边界一致后发布暂停状态。

任一必要保存失败都保留其错误，不能发布“可继续”的预算暂停。原生 best-effort API 保持存在，但它不足以满足本设计的持久接续验收。普通 `delta + error` 在现有 Graph 中会丢弃 delta；新代码禁止使用该出口表达任何上述受控停止。

## 7. Weave journal 联合契约

当前接点是 `internal/kernel/loomruntime/member_journal.go` 的 `memberBeforeStep`、`operation`、`restoreOperationUsage`，以及 `member_checkpoint.go` 的 `writeBoundaryTx` / `putTx`。源码已确认以下事实：

- `memberBeforeStep` 把 cursor 设为 0；同一未完成步骤保留 `__member_step_segment`。
- operation key 为 `member_run_id / segment / cursor`，并校验 kind 和输入摘要。
- 已确认 operation 返回原响应并恢复原 usage receipts；未知工具效果禁止自动重放。
- 每次 operation 的检查点保存 Graph 步骤入口状态及 journal 游标，不含 ToolLoop 局部新消息队列。
- 普通 `memberAfterStep` 当前将步骤标为完成；当前 `MemberRunner` 和工作流结果消费也没有额度暂停分支。

预算片段和 journal segment 是两个概念。首批额度安全点会同时结束旧预算片段和旧 journal segment；片段内崩溃不会建立新 segment。

### 7.1 片段内崩溃

片段入口保存 `(entry messages, entry control counters, segment ID, policy)`。从该入口开始，model、compaction model 和 tool operations 的回执顺序保持既有规则。

崩溃后以同一 member、同一 segment、cursor 0 重放。ToolLoop 从入口计数开始，逐个消费原 model/tool 响应，重建消息、重复计数和已用轮数；只有回执末尾之后才允许真实新调用。不能同时加载“已经增加的最新局部计数”又重放入口回执，否则会重复扣轮数。也不能跳过前几轮而保留 cursor 0，否则下一 model request 会与第一条旧输入摘要冲突。

`__member_journal_cursor` 是操作边界证据，不直接变成 ToolLoop 的循环起始值。Host 的 usage accumulator 继续按原 physical receipt 恢复和去重；局部循环计数的重建不能再次向账本记账。丢失模型回包的物理消耗缺口继续标为 incomplete。

### 7.2 保存受控暂停

`memberCheckpointStore.Put` 在现有 parent/attempt guard 事务内识别并验证暂停快照。仅当全部已派发工具都有确定回执、沒有 pending tool call、没有 fatal/unknown effect，才写入：

- 当前 ToolLoop 的完整私有快照、累计计数和原因；
- 原 journal segment ID 及最后确认的 cursor；
- immutable 暂停边界回执，绑定 member、policy、checkpoint seq 和快照摘要；
- latest、history 和 `weave_workflow_member_runs.checkpoint_seq`。

这些记录须一次事务提交。可使用既有事务 KV Store 的 `member-continuation:<workspace>` 命名空间保存边界与授权回执，不新建调度表。候选 key 为 `member_run_id / boundary / checkpoint_seq`。存储适配只能附加／核验接续元数据，不能悄悄改写 Graph 已序列化的快照字节。

暂停结果与终态结果分开保存。`MemberRunner` 识别已保存暂停后释放当前 attempt，并保留可接续记录；不能写入当前永久缓存终态的 `result` 字段。无授权的轮询读取这份暂停记录，不进入 `Graph.Resume`。

### 7.3 授权与新 journal segment

宿主先核对剩余总额度、可核验进展、无未核对效果，再持久保存 `ToolLoopResumeGrant`。授权 ID 必须唯一，绑定原 member 身份、原暂停边界和绝对总上限；相同 ID 相同载荷重放已有授权结果，相同 ID 不同载荷拒绝。并发授权通过当前 member 行锁和期望 checkpoint seq 串行裁决。

`PrepareToolLoopResume` 保留私有消息、staged patch、usage、累计轮数及重复计数；设置下一 `slice`、`slice_start_round=total_rounds_used`、`slice_rounds_used=0`。原 frozen bundle 和初始输入身份不变，追加额度是独立授权回执。

开始下一次 Graph.Resume 时，`memberBeforeStep` 在任何新效果之前，以同一 parent/attempt guard 事务完成：

1. 核验授权尚未应用、其 source checkpoint 仍是当前已保存暂停边界。
2. 创建由授权 ID 派生的确定性 journal segment ID，并把 cursor 初始化为 0。
3. 保存新片段入口快照和 segment-start 回执，绑定旧边界、授权 ID、policy 与累计用量。
4. 原子写 latest/history/checkpoint seq，并把该授权标记为已应用到这个新入口。

提交后才允许第一个新 model/tool operation。新入口使用最新私有消息，因此必须使用新 journal segment；不能沿用旧 segment 的 cursor 0。

若在授权保存后、入口事务前崩溃，没有新效果，重试同一授权。若入口事务已提交但尚未执行，恢复该 active segment，不能重新增加 slice。若新片段中途崩溃，按 7.1 从新入口回放。若暂停或授权回包丢失，按持久回执返回既有事实，不增加额度或另建 member。

### 7.4 产品与发布接点

Weave 发布版本增加控制模式开关和初始总轮数，编译器把当前硬编码的 20 轮显式写入版本配置，参与 frozen identity。`internal/kernel/compiler/compiler.go`、frozen 编解码/校验、`member_journal.go`、`member_checkpoint.go`、`member_run.go` 与工作流停止结果映射必须成批接通，不能只更新 Loom 依赖。

发布/组合层根据已知 frozen 配置，结合既有 `MayYield` 能力检查与串行拓扑约束，在执行前只准入已接通的单个 leaf 路径；含受控 ToolLoop 的未适配嵌套组合必须在任何父子图 Run 之前被拒绝。这是宿主掌握配置后承担的合同，不能从 Loom 的不透明 Step 闭包或可选拓扑描述自动证明。

Weave 的暂停状态区分可候选接续、待核对、总额度已尽和执行失败。自动接续仍由现有串行成员运行权威决定；模型输出不能签发预算授权。原已错误保存为成功的历史成员保持原记录，不能靠启用本设计自动复活。

本批先完成 Weave 团队所需的停止与接续能力。Workbench 自身 harness 的能力收束和专用监督配置留到后续独立改动，不在本批实施 DSH 替换或收束。Weave 继续作为业务运行权威，Loom 只提供成员内部机制；本设计不向 Workbench/前台 agent-loop 增加新的业务执行或接续职责。

图级补丁 `60ef013` 继续保留在独立 Loom worktree。只有本节列出的 Weave 接点和联合恢复验证整体就绪，才升级 Weave 依赖并启用控制模式。

## 8. Layer 0 替代方案与成本

| 方案 | 必要工作 | 取舍 |
|---|---|---|
| 现有 yield，加 stdlib 结构化记录（推荐） | ToolLoop 控制字段、严格解码/恢复助手、宿主暂停 hook 分流及 journal 接续 | Layer 0 不变；顶层 `StopYielded` 与具体 ToolLoop 原因分开读取；复用已验证的合并、checkpoint、token、phase |
| 新增显式 `StepStop{Reason, Phase, Cause}` | 上述持久状态和 journal 工作全部保留；另外修改 root error 协议、Graph 分支、保存失败语义、hooks 与嵌套传播测试 | 若产品合同明确要求 ToolLoop 在顶层返回 `StopBudget` 或独立受控原因，可单独采用；这项要求目前不是完成接续的必要前提 |
| 普通 error 加部分增量 | 需要改变所有普通 error 的 delta 合并语义，或复制内核保存逻辑 | 不接受；扩大原有 Step 错误契约且无法区分有效现场 |
| 将每轮拆成新的图节点 | 改图拓扑、发布身份、operation 映射与恢复基准 | 首批成本最大；本问题不需要新的调度原语 |

显式 stop 的可行最小版本仍可保留原 Step 签名，但必须是只能由当前 Graph 消费一次的 marker；Graph 返回后不再把 marker 包在 error 链中，避免子图 error 被父图误当成自己的受控增量。marker 要在普通错误分支之前校验、合并、必需保存，保存失败不能降级。这里仅记录备选约束，不把该 API 与推荐方案同时实施。

复用 yield 仍需明确嵌套限制。当前 `stdlib.runChildContinuation` 对 child resume 的校验围绕审批 tool results，不应声称已经支持本设计的预算授权。通过 `NewSubGraphStep` 或 `NewHandoffStep` 到达受控 ToolLoop 时，stdlib 只在该 ToolLoop 入口拒绝，保证该步骤没有新的 model/tool 调用。若子图先执行 legacy ToolLoop 再到达受控 ToolLoop，前者的模型调用已经发生，局部保护既不能阻止也不能回滚它。

因此，全拓扑拒绝必须由 7.4 的宿主预检在执行前完成。Loom 不为此向 Layer 0 增加 ToolLoop 能力元数据，也不从函数指针建立注册表。后续若需要嵌套支持，再扩展既有 child continuation 适配，仍不改变 Graph 单跳串行结构。

## 9. 现有共享预算并发入口核查

当前没有找到把同一个已安装预算 counter 的 context 并发传入多个 Graph 的现成执行链。

- Loom `graph.go` 在单个串行循环中执行 Step；`stdlib/steps.go` 的 child Run/Resume 是同步调用。
- `stdlib/toolloop.go` 的并发位于只读 `ToolDispatcher.Dispatch`，现有代码中没有从这条分支启动共享预算子图的工具实现。
- CLI 的 `cmd/loom/run.go` 是一次顶层 Graph.Run。
- 并发和 soak 测试使用各自的 `context.Background()` 发起根图；每次根图自行安装 counter，不共享已扣减的父 context。
- Weave `MemberRunner` 进入一张成员图；当前 durable member 入口明确要求串行 owner，工具定义在 journal 包装中统一改为串行派发。已有 nested adapter 同步调用 `NewSubGraphStep`。heartbeat goroutine 不执行 Graph。

因此未复现真实并行入口造成负余额快照，不扩大 `60ef013`。未来若明确加入共享 context 的并发子图，需要对 reservation/checkpoint 的并发一致性另行验证；这个 API 层面可能性不算当前运行路径证据。

## 10. 实施顺序和验收

主线程已确认 `StopYielded + ToolLoopOutcome` 合同。实施顺序如下，前两项属于本批 Loom 改动，后两项尚未开始：

1. stdlib 状态解码、结果读取和纯恢复转换接口；更新 API surface 基线。
2. 可选择的 ToolLoop 控制路径及默认行为兼容测试。根内核和 Step 签名不改。
3. Weave frozen 配置、完成 hook 分流、暂停边界与授权回执、新 journal segment 的原子入口。
4. 联合确定性恢复验证；通过后才启用一个隔离的真实成员样本。

必须通过的矩阵：

| 场景 | 断言 |
|---|---|
| 旧默认配置 | 既有 ToolLoop/park/StatePatch 输出及调用次数保持 |
| 需要三轮、片段两轮、总额三轮 | 两轮后保存完整现场并 `slice_limit`；无授权恢复零新调用；授权后仅执行第三轮 |
| 需要三轮、总额两轮 | `total_limit`；重复恢复不增加调用；绝对上限追加为三后才可继续 |
| 授权重试／并发／旧 token | 同一授权最多建立一个新入口；冲突或旧身份无效果 |
| 两轮中的每个 model/tool 回执边界崩溃 | 同一 segment 从入口重放，真实工具效果和 usage receipts 不重复 |
| 授权已保存／新入口已保存／暂停已保存但回包丢失 | 恢复各自已提交边界，不重置累计计数或新建 member |
| park 消费最后一轮 | 补齐真实结果后立即暂停，不凭审批恢复得到新轮数 |
| 重复批次跨两个片段 | 保留 hash/count，阈值批次无真实工具调用 |
| `length` 携带文字或工具调用 | 两者都保存 `provider_length`；工具不执行；无伪造 ToolResult |
| staged patch 加预算暂停 | 暂停保留私有 patch，公共状态未提交；正常完成仅提交一次 |
| Required 保存失败／损坏控制字段／policy 不同 | 不发布可继续状态，不执行新效果 |
| SubGraph／Handoff 的 legacy ToolLoop → 受控 ToolLoop | 前置 legacy 模型调用保持 1 次；受控步骤模型与工具调用均为 0，明确不提供全子图零效果保证 |
| 正常循环结束但交付缺项 | 机制完成不自动变成交付通过或用户接受 |

本设计的纯度证明为：实现主体位于 Layer 2 与 Layer 3，推荐路径不修改 Layer 0；不向内核引入业务词汇；默认路径不变；Step 仍仅返回增量，引擎仍唯一合并，路由仍确定且单跳。若主线程改选显式 stop，需要单独完成对应 Layer 0 review 后再实施。
