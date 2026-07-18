# 织机的构造：Loom 架构详解

> 一台织机只有三个活动部件——经线、纬线、梭子——却能织出任何花纹。

写 agent 框架的人很多，缺的从来不是功能，而是一个你能真正"拥有"的框架：出了问题能读懂、要改行为能下手、想嵌进自己系统时不用供着一个庞然大物。Loom 的答案是把整个内核压到约七百行 Go、五个类型定义。不是因为它做得少，而是因为它相信一件事：**成熟复杂的系统，必须有一个精瘦的内核。复杂性应该来自组合，而不是预先焊死在框架里。**

这五个类型是：

```go
type State  map[string]any                                           // 数据
type Step   func(ctx context.Context, state State) (State, error)    // 计算
type Router func(ctx context.Context, state State) (string, error)   // 控制流
type Store  interface { Get; Put; Delete; List; Tx }                 // 持久化
type Graph  struct { steps; routers; Run(); Resume() }               // 编排
```

没有 `Agent` 类，没有 `Chain` 抽象，没有 `Memory` 基类。从单个聊天机器人到上百个 agent 的编排，全部由这五个原语组合而成。

下面顺着织机的比喻，把每个部件拆开看。

## 一、经线：State

经线是绷在织机上、贯穿整块布的那组线。在 Loom 里，这就是 `State`——一个 `map[string]any`，要求所有值可以 JSON 序列化。它从图的入口一路流到出口，每个步骤都在它上面织入自己的一行。

`State` 的关键设计不在类型本身，而在**合并语义**。Step 不直接修改传入的 state，它返回一个"增量"；引擎调用 `State.Merge` 把增量合入，并且每次合并都产出一张新 map：

```go
state = state.Merge(update, g.mergeConfig)
```

合并策略按 key 注册。默认策略是覆盖（`Overwrite`），但对话消息这样的键需要追加而不是覆盖，所以 stdlib 的推荐配置是：

```go
mc := loom.NewMergeConfig()
mc.Register("messages", loom.AppendSlice)   // 消息累积，不覆盖
```

内置策略还有 `SumInt`、`SumFloat`（比如累加 token 用量），也可以注册任意自定义策略。`MergeConfig` 一旦挂到 Graph 上就被冻结，运行中再注册会直接 panic——策略是拓扑的一部分，不允许中途变卦。

"Step 返回增量、引擎负责合并"这个约定看似小事,却是后面一切能力的地基:因为 state 在任何步骤边界上都是一份完整、自洽的快照,所以它随时可以被序列化落盘——这就是 checkpoint;随时可以被反序列化重新装上织机——这就是 resume。

## 二、纬线：Step

纬线是一次织入一行的线。Loom 的 Step 是整个框架里最短的文件，[step.go](../step.go) 一共八行，有效定义一行：

```go
type Step func(ctx context.Context, state State) (State, error)
```

就是一个函数。读 state，做事（调 LLM、跑工具、查数据库、什么都行），返回增量。它不知道自己在图的哪个位置，不知道前后是谁，不持有任何框架对象。这意味着：

- **测试一个 Step 不需要框架**——构造一个 map，调函数，断言返回值；
- **任何函数都能当 Step**——包括另一个 Graph（后面会讲到子图就是这么做的）；
- **框架对 Step 的全部要求就是这个签名**——没有基类要继承，没有生命周期方法要实现。

## 三、梭子：Router

梭子决定纬线织完这一行之后往哪边折返。Router 的签名和 Step 几乎对称：

```go
type Router func(ctx context.Context, state State) (string, error)
```

返回下一个步骤的名字，返回空字符串则停机。[router.go](../router.go) 提供四个构造器，总共六十来行：

```go
loom.Always("chat")                          // 无条件去 chat
loom.End()                                   // 停机
loom.Branch("intent", map[string]string{     // 按 state 键的值分支
    "refund":  "refund_flow",
    "consult": "chat",
}, "chat")
loom.Condition(pred, "approve", "reject")    // 按谓词二分
```

这里藏着 Loom 和"让模型决定下一步"式框架的根本分歧：**路由是代码，不是提示词。** `Branch` 查表，`Condition` 跑谓词，同样的 state 永远走同样的边。模型可以往 state 里写值来*影响*路由（比如写 `__delegate_to`），但*判定*本身是确定性的、可测试的、可复现的。流程里哪些环节绝不能跳过、哪些分支绝不能走错，就编在拓扑里，模型无从违反。

## 四、机架：Graph 的主循环

State、Step、Router 是活动部件，Graph 是把它们绷在一起的机架。构建 API 只有两个动作：

```go
g := loom.NewGraph("agent", "guard")          // 图名 + 入口步骤
g.AddStep("guard", guardStep, loom.Always("chat"))
g.AddStep("chat",  chatStep,  loom.End())
```

`AddStep` 的第三个参数就是这个步骤的"出边"——一个 Router。整个执行引擎是 [graph.go](../graph.go) 里 `Run` 方法的一个 for 循环，不到一百行，值得逐段读：

```
for 当前步骤非空 {
    1. 熔断检查（per-graph maxIter，默认 100）
    2. 全局步数预算检查（跨子图共享的 atomic 计数器）
    3. Before hooks
    4. 执行 Step，Merge 增量
    5. After hooks
    6. checkpoint 落盘
    7. 检查 __yield（人机协作暂停点）
    8. 调 Router 决定下一步
}
```

几个细节体现了这个循环的成色：

**双层熔断。** 每张图有自己的 `maxIter`（防单图死循环），另有一个可选的全局步数预算 `WithStepBudget(n)`——它是一个 atomic 计数器，塞在 context 里往下传，父图和所有子图共享同一份余额。多层嵌套的编排跑飞时，任何一层扣完预算都会停机。

**错误不丢现场。** Step 返回错误时，引擎先把 `__error` 和 `__failed_step` 合入 state，尽力做一次 checkpoint，然后才返回。事后排查时，现场就在库里。

**每步落盘。** 只要传了 Store，每个步骤执行完引擎都会把 `{run_id, graph, last_step, state, yield_phase}` 序列化写入。落盘策略可选：`CheckpointBestEffort`（默认，失败只记日志）或 `CheckpointRequired`(失败即停机)——前者适合"能跑就跑"的场景，后者适合"宁停不丢"的审计场景。

**结束有名有姓。** 执行结果的 `StopReason` 明确区分六种停机原因：`completed`（正常走完）、`yielded`（等人）、`max_iter`（熔断）、`budget`（预算耗尽）、`error`（步骤报错）、`hook_abort`（钩子拦停）。调用方不需要靠猜。

Graph 还支持 `SetHooks` 挂前后钩子（每步执行前后各跑一遍，返回错误即停机——stdlib 的预算控制就是这么实现的），以及 `SetTopology` 声明拓扑供外部可视化。

## 五、停机与续织：yield / resume

织机最实用的能力之一：随时停机，改天接着织，花纹不乱。

Loom 的人机协作（human-in-the-loop）没有专门的机制，它只是三个已有部件的组合：一个 state 键、一次 checkpoint、一次重新入场。

任何 Step 想暂停，往返回值里写 `__yield: true` 即可。引擎在 checkpoint 之后检查到这个键，就带着 `StopReason: "yielded"` 返回。此时完整的 state 已经在 Store 里了，进程可以退出、机器可以重启。等审批通过：

```go
result, _ = g.Resume(ctx, runID, loom.State{"approved": true}, store)
```

`Resume` 做的事非常朴素：从 Store 读回 checkpoint，把新输入 merge 进去，然后**把图的入口改成上次停下的步骤，再 Run 一遍**。没有魔法，恢复就是换个入口重新执行。

这里有个精巧的约定——`yield_phase`。停在"步骤中间"（`mid_step`）还是"步骤之后"（`after_step`），决定恢复时是重跑该步骤还是直接走它的路由。stdlib 的 `HumanWaitStep` 用的是 `mid_step`，配合幂等设计：

```go
func NewHumanWaitStep(promptKey, responseKey string) loom.Step {
    return func(_ context.Context, state loom.State) (loom.State, error) {
        if _, ok := state[responseKey]; ok {
            return loom.State{}, nil          // 回复已在，放行
        }
        return loom.State{"__yield": true, "__yield_phase": "mid_step"}, nil
    }
}
```

恢复后重跑同一个步骤，它发现人的回复已经 merge 进 state，就直接通过。等待和放行是同一段代码的两次执行。

## 六、暗号：双下划线协议

读到这里你会注意到一批 `__` 开头的键：`__run_id`、`__yield`、`__yield_phase`、`__error`、`__failed_step`、`__system_prompt`、`__delegate_to`……这是引擎与步骤之间的全部"暗号"。

值得强调的是它们的实现方式：**就是普通的 state 键。** 没有隐藏通道，没有框架私有的上下文对象。这带来两个直接的好处：一是任何步骤、任何外部程序都能通过读写 state 参与这些协议（比如一个工具往 state 写 `__delegate_to` 就能触发确定性委派）；二是这些协议本身也随 checkpoint 一起落盘，恢复现场时协议状态不丢。

## 七、Store：五个方法的持久化

```go
type Store interface {
    Get(ctx, ns, key) ([]byte, error)
    Put(ctx, ns, key, value []byte) error
    Delete(ctx, ns, key) error
    List(ctx, ns, prefix) ([]string, error)
    Tx(ctx, fn func(Store) error) error
}
```

带命名空间的 KV，外加一个事务包裹。就这些。checkpoint、会话历史、任何业务数据，都走这五个方法。仓库自带三个实现：`memstore`（内存，测试用）、`pgstore`（PostgreSQL，生产用）、以及 CLI 里的 `filestore`（单机文件）。要接 Redis、SQLite 或者公司内部的存储，实现五个方法即可。

## 八、contract：内核不知道 LLM 的存在

这可能是 Loom 结构上最反直觉的一点：**在 graph.go 里搜不到 "LLM" 这个词。** 内核五件套对大模型一无所知——它编排的是"步骤"，至于步骤里是调模型、跑脚本还是查库，内核不关心。

所有与 LLM 相关的抽象集中在 `contract` 包，而且是纯接口、零实现：

```go
type LLM interface {
    Chat(ctx, ChatRequest) (*ChatResponse, error)
    Stream(ctx, ChatRequest) (<-chan StreamChunk, error)
}
type ToolDispatcher interface {
    ListTools(ctx) ([]ToolDef, error)
    Dispatch(ctx, ToolCall) (*ToolResult, error)
}
```

外加消息、工具调用、用量统计等数据类型，以及三档 `EffortLevel`（low/medium/high，v1.4 起随请求透传给支持推理档位的模型）。`provider/` 下的 OpenAI 兼容实现和 DeepSeek 实现都只依赖 contract。换供应商是换一个接口实现，不是改框架。

工具层面还有一个值得一提的声明式字段：`ToolDef.ReadOnly`。它是一个提示——这个工具只读、无副作用。后面会看到工具循环利用它做自动并行。

## 九、stdlib：预制的织法

内核提供织机，stdlib 提供常用的织法——一批开箱即用的 Step 和 Hook。它们全部构建在公开 API 之上，没有用到任何内核私有能力；换句话说，**stdlib 能做到的，你的代码也能做到。**

### ToolLoop：一个循环里的工程沉淀

`NewToolLoopStep` 是用得最多的预制件：LLM → 工具调用 → 结果回填 → 再问 LLM 的经典循环。看似简单，里面沉淀了几处实战工程：

- **读写分流。** 一批工具调用里，标了 `ReadOnly` 的并行执行，有副作用的按序串行。并行度不需要调用方操心，由工具声明驱动。
- **循环检测。** 对每批工具调用做 SHA-256 哈希，同一批调用连续重复 N 次（默认 3）就判定为死循环，主动收束。
- **优雅收尾。** 迭代预算耗尽时不抛错——因为一个裸错误最终会砸在最终用户脸上。它注入一条收尾指令，做最后一次*不提供任何工具*的调用，让模型只能用文字总结已完成的部分、说明剩下的怎么办。
- **上下文压缩。** 可选的 `CompactionPolicy`：自定义触发条件与摘要器，历史过长时自动压缩，压缩前可归档全文。
- **工具钩子。** `ToolHook` 带匹配器，Pre 可改写或拦截调用，Post 可审计结果。权限、审计、限流都从这里挂进去。

### 权限：deny / ask / allow

`PermissionDispatcher` 是个装饰器，包住任何 `ToolDispatcher`，按三级规则过滤：`deny` 直接拦；`allow` 非空时构成白名单；最有意思的是 `ask`——工具**不执行**，返回一个 `needs_confirmation` 结果，让模型向用户复述它想干什么、等用户明确同意后再调一次。确认流程本身就走对话，不需要额外的 UI 协议。

### 预算：把钱和 token 挂在钩子上

`CostBudgetHook(maxUSD)` 和 `TokenBudgetHook(maxTokens)` 是两个 After-Step 钩子，累计每步的用量，超限返回错误——引擎随即以 `hook_abort` 停机。防跑飞 agent 的最后一道闸，十几行代码。

### 提示词：分层组装，预算优先

`NewPromptAssembleStep` 按五层组装系统提示词，在 token 预算（默认 8000）内逐层装入：

1. 核心身份（永远在）；
2. 技能索引——所有技能的名字加一句描述（永远在，模型至少知道自己会什么）；
3. 扩展身份（预算够才装）；
4. Profile 覆盖层（按激活的角色追加）；
5. 命中的技能正文（关键词匹配，每轮最多两条）。

技能匹配器是可插拔接口，内置的 `KeywordMatcher` 对中日韩文本做了单字加二元组切分——不依赖空格分词，中文描述一样能命中。

### 子图与移交

`NewSubGraphStep` 把一整张 Graph 包成一个 Step——这就是"任何函数都能当 Step"的直接兑现，多 agent 编排因此不需要新概念。子图暂停时怎么办由 `YieldPolicy` 决定：`YieldBubble`（把 yield 连同子图的 run_id 向上冒泡，父图跟着停）、`YieldTrap`（视为错误）、`YieldCustom`（自定义处理）。`NewHandoffStep` 则是带上下文压缩的移交：把对话历史压缩后交给另一张图接管。

### 流式输出：一个装饰器的事

`NewStreamingLLM(inner, sink)` 包住任何 LLM 实现：`Chat` 被改写为内部驱动供应商的流式接口，每个增量实时推给 `StreamSink`，最终仍返回完整拼装好的响应。调用方（比如 ToolLoop）完全无感，引擎一行未改——流式是装饰出来的,不是引擎特性。

## 十、从声明到拓扑：specloader 与 compiler

以上都是"手工织"。Loom 还支持声明式的一条路：`specloader` 从目录里装载 `AgentSpec`——身份文件（按第一个 `##` 分核心/扩展两档）、`skills/` 下的 SKILL.md、profiles、以及子 agent 声明；`compiler.CompileAgent` 把 spec 编译成一张可运行的图：

```
无子 agent:    prompt_assemble → chat → END
有子 agent:    prompt_assemble → chat ──[Branch on __delegate_to]──> sub_<name> → END
```

编译出来的编排是**确定性**的：模型（或某个工具）往 state 写 `__delegate_to`，一个查表的 `BranchFunc` 把它映射到对应的子图步骤；查不到就正常结束。委派给谁由模型提议，但"提议如何生效"是代码。同时编译器给图配上 `WithMaxIterations(50)`、消息累积的 MergeConfig、可选的循环检测钩子，并调用 `SetTopology` 把图形状导出给宿主做可视化。

这条声明式路径也回答了一个常见问题：**agent 的"行为规范"到底放哪？** Loom 的答案是分两层。柔性的部分——语气、风格、判断标准——写在 SKILL.md 里，注入提示词，靠模型遵循；刚性的部分——必须先过守卫、必须有人审批、最多循环多少次、只能委派给谁——编进拓扑，由引擎保证。想让某条规则从"建议"升格为"铁律"，做法不是把提示词写得更凶，而是把它变成一个 Step 或一条边。因为内核足够小，这次"升格"的成本也足够低。

## 十一、从库到引擎：loom CLI 与 weave

Loom 首先是个库，但仓库同时发布一个独立的 agent 引擎 `cmd/loom`：stdin 进一段 prompt JSON，跑一轮 agent，stdout 出 NDJSON 事件流。它整合了 MCP 工具服务器接入（含初始化握手、必需服务器的启动前预检）、会话恢复、以及上面的 spec 编译编排——这正是姊妹项目 [weave](https://github.com/jinyitao123/Weave) 守护进程的 spawn-harness 后端。宿主进程不需要懂 Go，只要会读写行分隔的 JSON，就能驱动一个完整的 agent。

这也是对"库优先"设计的一次自我验证：CLI 没有用到任何内核不对外的能力，它就是这个库的一个普通用户。

## 尾声：结构即主张

回头看整个仓库的分层：

```
内核（~700 行）   State / Step / Router / Store / Graph —— 只做编排
contract          纯接口 —— 内核与 LLM 世界之间唯一的桥
stdlib            预制件 —— 全部构建在公开 API 上
provider / pgstore  可替换的外设
cmd/loom          用这个库写成的一个产品
```

每一层只依赖它下面的层，而且下层不知道上层的存在。这个结构本身就是 Loom 的核心主张：框架的价值不在于替你做了多少事，而在于它划定的边界是否干净——干净到你可以在一个下午读完内核，然后放心地在它之上织出自己的花纹。

一台织机只有三个活动部件。花纹的复杂，从来不是织机的复杂。
