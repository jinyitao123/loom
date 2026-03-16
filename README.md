<p align="center">
  <b>L O O M · 织 机</b><br/>
  <i>五个原语，编织任意 Agent 模式</i>
</p>

<p align="center">
  <a href="https://go.dev"><img src="https://img.shields.io/badge/Go-1.24+-00ADD8?logo=go" alt="Go"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-blue.svg" alt="License"></a>
  <img src="https://img.shields.io/badge/Kernel-584_LOC-brightgreen" alt="Kernel LOC">
</p>

---

织布机只有三个部件——经线、纬线、梭子——却能织出任何图案。

Loom 是同样的思路：**整个内核只有 5 个类型定义**，但它们组合起来，能表达从单轮聊天到百 Agent 编排的一切模式。不是框架，是引擎。

```go
type State  map[string]any                                           // 数据流
type Step   func(ctx context.Context, state State) (State, error)    // 计算
type Router func(ctx context.Context, state State) (string, error)   // 控制流
type Store  interface { Get; Put; Delete; List; Tx }                 // 持久化
type Graph  struct { steps; routers; Run(); Resume() }               // 编排
```

就这些。没有 `Agent` 类、没有 `Chain` 抽象、没有 `Memory` 基类。一切高级功能都从这五个原语**组合**而来，而不是从框架**继承**而来。

## 这意味着什么

<table>
<tr>
<td width="50%">

**你想做一个带工具的 Agent？**

三个 Step 串起来就是。

</td>
<td>

```go
g := loom.NewGraph("agent", "guard")
g.AddStep("guard", guardStep, Always("chat"))
g.AddStep("chat",  toolLoop,  End())
```

</td>
</tr>
<tr>
<td>

**你想让 Agent 暂停等人类审批？**

Step 返回 `__yield: true`，Graph 自动冻结状态。人类批准后一行 `Resume()` 继续。

</td>
<td>

```go
// Agent 暂停
result, _ := g.Run(ctx, input, store)
// result.StopReason == "yielded"

// 人类审批后恢复
result, _ = g.Resume(ctx, result.RunID,
    State{"approved": true}, store)
```

</td>
</tr>
<tr>
<td>

**你想编排 10 个 Agent 协作？**

每个 Agent 是一个 Graph，用 `SubGraphStep` 嵌进父 Graph。共享同一套 checkpoint，共享同一个 step budget。

</td>
<td>

```go
parent := loom.NewGraph("orchestrator", "dispatch",
    WithStepBudget(500)) // 所有子图共享

parent.AddStep("dispatch", router, Branch(...))
parent.AddStep("analyst", SubGraphStep(analystGraph), ...)
parent.AddStep("coder",   SubGraphStep(coderGraph),   ...)
```

</td>
</tr>
<tr>
<td>

**你的 Agent 进程崩了？**

不用做任何事。每一步执行完自动 checkpoint 到 PostgreSQL，重启后 `Resume()` 从断点继续，不丢一步。

</td>
<td>

```go
// 崩溃前：step A → B → C ✓ → [crash]
// 重启后：
result, _ = g.Resume(ctx, runID, State{}, pgStore)
// 从 step D 继续，C 的状态完整保留
```

</td>
</tr>
</table>

## 内核不知道的事

这是 Loom 最重要的设计决策——**内核刻意不知道这些东西**：

| 内核不知道 | 所以你可以 |
|---|---|
| LLM 是什么 | 用 OpenAI、Claude、DeepSeek、本地模型——Step 里随便换 |
| MCP 是什么 | 工具协议随便接，MCP / A2A / 自定义 RPC 都行 |
| Memory 怎么存 | 用 RAG、用图数据库、用全文检索——State 里放什么是你的事 |
| HTTP 怎么服务 | 你爱用 Gin、Echo、net/http 都行——Loom 是库，不是服务 |

内核只管一件事：**按照 Graph 定义的顺序执行 Step，沿途 checkpoint，遇到 yield 就暂停**。剩下的全是用户态。

## 分层架构

```
┌──────────────────────────────────────────────────┐
│  Layer 3 · Platform                              │  ← 你的应用：HTTP / Auth / 多租户
│  (独立仓库，不在 loom/ 里)                         │
├──────────────────────────────────────────────────┤
│  Layer 2 · Stdlib                    ~1800 LOC   │  ← 预制积木：ToolLoop / Guard / Handoff
├──────────────────────────────────────────────────┤
│  Layer 1 · Contract                   ~300 LOC   │  ← 纯接口：LLM / ToolDispatcher / Embedder
├──────────────────────────────────────────────────┤
│  Layer 0 · Kernel                     ~600 LOC   │  ← 五个原语，仅此而已
└──────────────────────────────────────────────────┘
   依赖规则：Layer N 只能 import Layer N-1 及以下。绝对的。
```

## Stdlib：组合出来的，不是写死的

Stdlib 里的每个组件都是 `Step` 或 `Router` 的组合——没有新原语，没有特殊通道。

```go
// ToolLoop：LLM 调用 → 工具执行 → 结果回传 → 再调 LLM，循环直到 LLM 说"完了"
chat := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
    MaxIterations: 20,
    Compaction:    &compactionPolicy,  // 上下文太长时自动摘要
    ToolHooks:     []contract.ToolHook{auditHook}, // 每次工具调用前后的钩子
})

// PermissionDispatcher：声明式工具权限，deny 优先于 allow
safeTool := stdlib.NewPermissionDispatcher(tools,
    []string{"rm_rf", "drop_table"},  // 永远禁止
    []string{"read_*", "search_*"},   // 只允许这些
)

// CostBudgetHook：跑到 $5 自动停，不怕 Agent 烧钱
g.SetHooks(loom.HookPoints{
    After: []loom.StepHook{stdlib.CostBudgetHook(5.00)},
})
```

**read-only 工具自动并发，stateful 工具严格串行**——ToolLoop 会读取 `ToolDef.ReadOnly` 标记自动判断，不需要你操心。

## 安装

```bash
go get github.com/jinyitao123/loom
```

## 快速上手

```go
package main

import (
    "context"
    "fmt"
    "github.com/jinyitao123/loom"
)

func main() {
    // 一个 Step 就是一个函数
    greet := func(_ context.Context, s loom.State) (loom.State, error) {
        name := s["name"].(string)
        return loom.State{"output": "Hello, " + name + "!"}, nil
    }

    g := loom.NewGraph("greeter", "greet")
    g.AddStep("greet", greet, loom.End())

    result, _ := g.Run(context.Background(), loom.State{"name": "World"}, nil)
    fmt.Println(result.State["output"])   // Hello, World!
    fmt.Println(result.StopReason)        // completed
}
```

## 适用于

- 需要 **crash recovery** 的长时间运行 Agent
- 需要 **人机协同审批** 的企业工作流
- 需要 **多 Agent 编排** 但不想引入重量级框架
- 需要 **预算控制**（token / 美元）防止 Agent 失控
- 需要在 Go 生态中嵌入 Agent 能力，而不是起一个 Python sidecar

## 与其他框架的区别

| | Loom | LangGraph | OpenAI Agents SDK |
|---|---|---|---|
| 语言 | Go | Python | Python |
| 内核大小 | ~600 LOC | ~15K LOC | ~3K LOC |
| 持久化 | 自动 checkpoint | 自动 checkpoint | 无（自己实现） |
| LLM 耦合 | 零（内核不知道 LLM） | 中（ChatModel 内置） | 强（OpenAI 绑定） |
| 工具协议 | 任意（合约注入） | LangChain Tools | OpenAI function calling |
| 子图 / 嵌套 | 原生支持 | 原生支持 | 不支持 |
| 人机协同 | yield / resume | interrupt | Handoff（有限） |
| 适合嵌入 | 是（Go 包） | 否（Python 服务） | 否（Python 服务） |

## 项目结构

```
loom/
├── graph.go          State × Step × Router → 执行引擎
├── state.go          可注册合并策略的类型化 map
├── step.go           一行：type Step func(ctx, State) (State, error)
├── router.go         控制流原语 + Always / Branch / Condition
├── store.go          5 方法持久化接口
├── options.go        GraphOption：merge / checkpoint / budget
├── memstore.go       内存 Store（测试用）
│
├── contract/         纯接口层：LLM / ToolDispatcher / Embedder
├── stdlib/           预制 Step & Hook 库
│   ├── toolloop.go   LLM ↔ Tool 循环，自动并发 read-only 工具
│   ├── steps.go      Guard / HumanWait / SubGraph / Handoff
│   ├── permission.go 声明式 deny/allow 工具权限
│   ├── budget.go     Token & 美元预算钩子
│   ├── prompt.go     分层 prompt 组装 + 技能匹配
│   └── session.go    会话历史持久化
│
├── pgstore/          PostgreSQL Store 实现
└── provider/         LLM Provider（OpenAI / DeepSeek）
```

## License

Apache-2.0
