<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/loom-logo-dark.png">
    <source media="(prefers-color-scheme: light)" srcset="assets/loom-logo-light.png">
    <img src="assets/loom-logo-light.png" alt="Loom" width="420">
  </picture>
</p>

<p align="center">
  <a href="README.md">English</a> | <a href="README.zh-CN.md">中文</a>
</p>

<p align="center">
  <a href="https://github.com/jinyitao123/loom/actions/workflows/loom-cli.yml"><img src="https://github.com/jinyitao123/loom/actions/workflows/loom-cli.yml/badge.svg" alt="CI"></a>
  <a href="https://pkg.go.dev/github.com/jinyitao123/loom"><img src="https://pkg.go.dev/badge/github.com/jinyitao123/loom.svg" alt="Go Reference"></a>
  <a href="https://goreportcard.com/report/github.com/jinyitao123/loom"><img src="https://goreportcard.com/badge/github.com/jinyitao123/loom" alt="Go Report Card"></a>
  <a href="https://github.com/jinyitao123/loom/releases"><img src="https://img.shields.io/github/v/release/jinyitao123/loom" alt="Release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-blue.svg" alt="License: MIT"></a>
</p>

Loom 是一个 Go 库，用来写那种需要中途停下来等人审批、过几天接着跑、进程崩了也不丢进度的 agent 工作流。

在 Loom 里，一个 agent 是一张明确的步骤图，跑在一个可以检视的 `State` 上。引擎按图执行，每走一步通过可插拔的 `Store` 自动存档（checkpoint），随时可以冻结（`yield`），之后在另一个进程、另一天用 `Resume` 原样续跑。有一件事它**不**保证：step 是你自己写的函数，里面调了 LLM、读了时钟，可复现性就取决于你自己。Loom 守住的是拓扑、执行顺序和故障恢复——剩下的归你。

当前版本 `v0.8.0`，1.0 之前 API 只做增量变更，破坏性改动会伴随 minor 版本号和迁移说明。需要 Go 1.24+，MIT 协议。

## 快速开始

```bash
go get github.com/jinyitao123/loom   # 在你的 Go module 里执行
```

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/jinyitao123/loom"
)

func main() {
    greet := func(_ context.Context, s loom.State) (loom.State, error) {
        return loom.State{"output": "Hello, " + s["name"].(string) + "!"}, nil
    }

    g := loom.NewGraph("greeter", "greet")
    g.AddStep("greet", greet, loom.End())

    result, err := g.Run(context.Background(), loom.State{"name": "World"}, nil)
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(result.State["output"]) // Hello, World!
}
```

API 文档：[pkg.go.dev/github.com/jinyitao123/loom](https://pkg.go.dev/github.com/jinyitao123/loom)

## 这给你带来什么

**中途停下来等人审批。** 某个 step 置 `__yield: true`，图就带着存档冻结。几天后人做了决定，`Resume` 从停下的地方原样继续：

```go
result, err := g.Run(ctx, input, store)
// result.StopReason == "yielded"

result, err = g.Resume(ctx, result.RunID, loom.State{"approved": true}, store)
```

**进程崩了不丢进度。** 每一步自动存档。跑到 C 步崩了，重启，resume——从 D 步继续，C 的状态完好：

```go
result, err = g.Resume(ctx, runID, loom.State{}, pgStore)
```

新写入的 checkpoint 带 `schema_version: 1`：新二进制兼容读旧存档，旧二进制遇到新格式直接拒绝（fail closed）。`Resume`、`ResumeAt` 和历史读取遵循同一套规则。

## 事实

- 五个原语——`State`、`Step`、`Router`、`Graph`、`Store`。没有 `Agent` 基类，没有 `Chain` 抽象，没有 `Memory` 基类；其他能力全部由这五者组合而来，不是继承来的。
- 整个根包约 1,650 行、9 个文件（`wc -l`，含注释）。stdlib 约 3,100 行，contract 接口层约 200 行。
- 306 个测试、4 个 fuzz 目标；CI 在 Linux/macOS/Windows 三平台对 CLI 跑 race 检测，覆盖率门槛 85%。
- 核心库只有一个运行时依赖（`google/uuid`）；只有引用 `pgstore` 才会拉入 `pgx`。
- benchmark 在 `tests/` 里——内存存储下每步开销在微秒级。自己跑：`go test ./tests/ -run '^$' -bench=.`

## 内核刻意不知道什么

| 内核不知道 | 所以你可以 |
|---|---|
| LLM 是什么 | OpenAI、Claude、DeepSeek、本地模型——在 Step 里随便换 |
| MCP 是什么 | 接任何工具协议——MCP / A2A / 自定义 RPC |
| 怎么存记忆 | RAG、图数据库、全文检索——State 里放什么你说了算 |
| 怎么伺服 HTTP | Gin、Echo、net/http——Loom 是库，不是服务 |

内核只做一件事：按图的顺序执行 step、沿途存档、遇到 yield 暂停。prompt、工具、记忆、传输——都是你的领域，可以用 `stdlib` 的积木（tool loop、权限、预算、会话、子图）拼，也可以自己写。

## 什么时候别用 Loom

- **你想要开箱即用。** 内置 RAG、向量记忆、可视化编排、托管平台——这些 Loom 都没有，而且是刻意没有。要这些，LangGraph 或 OpenAI Agents SDK 更快。
- **你的技术栈是 Python 或 TypeScript。** Loom 只有 Go。
- **你要的是控制平面。** 多租户鉴权、审批界面、审计账本、可寻址的 agent——那是 [Weave](https://github.com/jinyitao123/Weave)（即将开源）在 Loom 之上做的事。Loom 自己始终是一个库加一个单机 CLI。

注意分层：*内核*不认识 LLM 和记忆；`stdlib` 提供会话和 prompt 工具；`loom` *CLI* 在其上加了 MCP 工具、会话续跑和语义记忆。

## 对比

| | Loom | LangGraph | OpenAI Agents SDK |
|---|---|---|---|
| 语言 | Go | Python | Python |
| 核心库体量¹ | ~1.6K LOC（根包 9 个文件） | ~27.9K LOC（`libs/langgraph/langgraph`） | — |
| 持久化 | 每步自动 checkpoint | Checkpointer（需配置） | 无内置 |
| LLM 耦合 | 零 | LangChain 生态 | OpenAI 优先 |
| 工具协议 | 任意 | LangChain tools | function calling |
| 子图嵌套 | 原生 | 原生 | Handoffs |
| 人在环中 | yield / resume，状态已存档 | interrupt | 有限 |

¹ 双方同一方法实测：`wc -l` 核心包、含注释，2026-07-30 测量（loom@`9f4c974`，langgraph@`4134145`）。

## `loom` CLI

仓库还附带 `loom` 二进制：一个基于这个库的独立 agent 引擎——stdin 读 prompt JSON，跑一轮 agent，stdout 输出 NDJSON 事件流。支持 MCP 工具服务、会话续跑、语义记忆，以及从 agent spec 编译出的子代理编排。

```bash
go install github.com/jinyitao123/loom/cmd/loom@latest

# 或从 GitHub Release 安装（linux / macOS）：
curl -fsSL https://raw.githubusercontent.com/jinyitao123/loom/main/install.sh | sh
```

事件格式见 [cmd/loom/README.md](cmd/loom/README.md)，宿主进程集成方法见 [docs/host-integration.md](docs/host-integration.md)，架构设计见 [docs/architecture.zh-CN.md](docs/architecture.zh-CN.md)。

## 项目结构

```
loom/
├── graph.go          执行引擎：State × Step × Router → Run / Resume
├── state.go          可注册合并策略的类型化 map
├── step.go           type Step func(ctx, State) (State, error)
├── router.go         控制流原语：Always / Branch / Condition
├── store.go          5 方法持久化接口
├── lifecycle.go      宿主观察 seam（allocation / terminal / checkpoint）
├── options.go        GraphOption：merge / checkpoint / budget
├── memstore.go       内存 Store（测试用）
│
├── contract/         纯接口层：LLM / ToolDispatcher / Embedder
├── stdlib/           预制 Step & Hook（tool loop、权限、预算、会话…）
├── pgstore/          PostgreSQL Store
├── provider/         LLM Provider（OpenAI 兼容 / DeepSeek）
├── cmd/loom/         `loom` CLI
├── tests/            黑盒测试套件（仅走公开 API）
└── docs/             宿主集成契约 & 编排设计
```

## 贡献

见 [CONTRIBUTING.md](CONTRIBUTING.md)。一句话版：五个原语、分层 import、内核零业务词——而且有 CI 护栏盯着。

## 许可证

[MIT](LICENSE)
