# L O O M · 织机

**五个原语，编织任意 Agent 模式。**

一个极简的 Go Agent 运行时内核——用 `Graph`、`Step`、`Router`、`State`、`Store` 五个原语，组合出从单轮聊天到百 Agent 企业级工作流的一切模式。

[![Go](https://img.shields.io/badge/Go-1.24+-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

---

## 为什么需要 Loom

2026 年的 Agent 框架已分化为两个阵营：

| 阵营 | 代表 | 取舍 |
|---|---|---|
| **图优先** | LangGraph | 强持久化、显式控制流，但抽象层厚重 |
| **极简优先** | OpenAI Agents SDK | API 轻便，但状态/持久化自己搞 |

Loom 取两者之长：**LangGraph 的持久化 + OpenAI SDK 的轻量**。内核 < 600 行 Go 代码，自动 checkpoint，crash recovery 开箱即用。

## 设计哲学

> 织布机只有几个运动部件——经线、纬线、梭子——却能织出任何图案。
> Loom 亦然：五个原语，组合出一切。

| 原则 | 含义 |
|---|---|
| **内核不知道 LLM 是什么** | LLM 调用是用户态 Step，不是内核概念 |
| **内核不知道 MCP 是什么** | 工具协议是 Layer 1 合约，由宿主注入 |
| **上层功能皆为组合** | HITL、Handoff、Guardrails、Memory——全由 5 个原语搭建 |
| **持久化是自动的** | 每一步自动 checkpoint，崩溃恢复免费 |
| **Loom 是引擎，不是整车** | 没有 HTTP server、没有 auth——那些是平台层的事 |

## 架构总览

```
Layer 0 · Kernel     ── State / Step / Router / Store / Graph      (~600 LOC)
Layer 1 · Contract   ── LLM / ToolDispatcher / Embedder / Hook     (~300 LOC)
Layer 2 · Stdlib     ── ToolLoop / Guard / Handoff / SubGraph ...  (~1800 LOC)
Layer 3 · Platform   ── HTTP API / Auth / 多租户 / 可观测性        (独立仓库)
```

**依赖规则是绝对的**：Layer N 只能 import Layer N-1 及以下。

## 五个原语

### State — 流经一切的共享状态

```go
type State map[string]any
```

一个 JSON-safe 的 map。Step 读取当前 State，返回增量——由可注册的 `MergePolicy` 决定如何合并（覆盖、追加、求和，或自定义）。

### Step — 原子执行单元

```go
type Step func(ctx context.Context, state State) (State, error)
```

一个函数。输入 State，输出增量 State。可以是 LLM 调用、数据库查询、HTTP 请求——内核不关心。

### Router — 决定下一步去哪

```go
type Router func(ctx context.Context, state State) (string, error)
```

返回下一个 Step 的名字，返回 `""` 则终止。内置 `Always`、`Branch`、`Condition` 等快捷构造器。

### Store — 持久化接口

```go
type Store interface {
    Get(ctx, ns, key) ([]byte, error)
    Put(ctx, ns, key, value) error
    Delete(ctx, ns, key) error
    List(ctx, ns, prefix) ([]string, error)
    Tx(ctx, fn func(Store) error) error
}
```

5 个方法，不多不少。内置 `MemStore`（测试）和 `PGStore`（生产，PostgreSQL 16+）。

### Graph — 将一切编织在一起

```go
g := loom.NewGraph("my-agent", "entry-step",
    loom.WithMergeConfig(loom.DefaultMergeConfig()),
    loom.WithStepBudget(500),
)
g.AddStep("entry-step", myStep, loom.Always("next-step"))
g.AddStep("next-step", anotherStep, loom.End())

result, err := g.Run(ctx, loom.State{"input": "hello"}, store)
```

Graph 是 Step + Router 的编排。自动 checkpoint，支持 yield/resume（人机协同），支持子图嵌套。

## Stdlib 亮点

| 组件 | 作用 |
|---|---|
| `NewToolLoopStep` | LLM → 工具调用 → 结果 → LLM 循环，read-only 工具自动并发 |
| `NewGuardStep` | 前置/后置安全检查 |
| `NewSubGraphStep` | 将子图当作一个 Step 运行，yield 可冒泡/捕获/自定义 |
| `NewHandoffStep` | 带上下文压缩的跨 Agent 委派 |
| `NewHumanWaitStep` | HITL 暂停，等待人类输入后 Resume |
| `NewPromptAssembleStep` | 分层身份 + 技能匹配，动态组装 system prompt |
| `PermissionDispatcher` | 声明式 deny/allow 工具权限 |
| `CostBudgetHook` | Token/USD 预算控制，超限自动终止 |
| `CompactionPolicy` | 上下文窗口自动摘要压缩 |

## 快速开始

```bash
go get github.com/anthropic/loom
```

### 最小示例：echo agent

```go
package main

import (
    "context"
    "fmt"
    "github.com/anthropic/loom"
)

func main() {
    echo := func(_ context.Context, s loom.State) (loom.State, error) {
        return loom.State{"output": fmt.Sprintf("echo: %v", s["input"])}, nil
    }

    g := loom.NewGraph("echo", "echo-step")
    g.AddStep("echo-step", echo, loom.End())

    result, _ := g.Run(context.Background(), loom.State{"input": "hello"}, nil)
    fmt.Println(result.State["output"]) // echo: hello
}
```

### 带工具循环的 LLM Agent

```go
g := loom.NewGraph("assistant", "assemble",
    loom.WithMergeConfig(stdlib.DefaultMergeConfig()),
)
g.AddStep("assemble", stdlib.NewPromptAssembleStep(promptCfg), loom.Always("guard"))
g.AddStep("guard",    stdlib.NewGuardStep(myChecks...),        loom.Always("chat"))
g.AddStep("chat",     stdlib.NewToolLoopStep(llm, tools, opts), loom.End())
```

### 人机协同 (HITL)

```go
g.AddStep("ask-human", stdlib.NewHumanWaitStep("prompt", "human_response"),
    loom.Condition(func(s loom.State) bool {
        _, ok := s["human_response"]
        return ok
    }, "process", "ask-human"),
)

// 第一次运行：yield
result, _ := g.Run(ctx, input, store)
// result.StopReason == "yielded"

// 人类提供输入后 resume
result, _ = g.Resume(ctx, result.RunID,
    loom.State{"human_response": "approved"}, store)
```

## 组合模式

Loom 的 5 个原语可以组合出所有主流 Agent 模式：

```
┌─────────────────────────────────────────────────┐
│  单 Agent        Guard → ToolLoop → End         │
│  HITL            Guard → Chat → HumanWait ↺     │
│  多 Agent        SubGraph(agentA) → Router →    │
│                    SubGraph(agentB) → End        │
│  Handoff         Chat → HandoffStep(target) →   │
│                    Router → End                  │
│  层级编排        Graph(orchestrator) 内嵌        │
│                    SubGraph(specialist) × N      │
└─────────────────────────────────────────────────┘
```

## 项目统计

| 指标 | 数值 |
|---|---|
| 内核代码量 | < 600 LOC |
| 标准库代码量 | ~1,800 LOC |
| 外部依赖 | `google/uuid`（内核）+ `pgx`（PGStore） |
| 生产存储 | PostgreSQL 16+ |
| 测试覆盖 | 95%+ |

## License

Apache-2.0
