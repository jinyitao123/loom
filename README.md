<p align="center">
  <img src="assets/loom-logo.png" alt="Loom" width="420">
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

Loom is a small Go library for agent workflows that need to pause for a human, resume later, and survive a crash.

An agent in Loom is an explicit graph of steps over an inspectable `State`. The engine executes the steps in graph order, checkpoints after each one through a pluggable `Store`, and can freeze mid-run (`yield`) and continue later (`Resume`) — on another day, in another process. What it does **not** guarantee: your steps are your functions, so a step that calls an LLM or reads a clock is as reproducible as you make it. Loom holds the topology, the execution order, and the recovery; the rest is yours.

Status: `v0.8.0`, pre-1.0. API changes are additive; breaking changes ship with a minor bump and migration notes. Requires Go 1.24+. MIT licensed.

## Quickstart

```bash
go get github.com/jinyitao123/loom   # inside your Go module
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

API reference: [pkg.go.dev/github.com/jinyitao123/loom](https://pkg.go.dev/github.com/jinyitao123/loom)

## What this buys you

**Pause for human approval.** A step sets `__yield: true`; the graph freezes with its state checkpointed. Days later, after the human decides, `Resume` picks up exactly where it stopped:

```go
result, err := g.Run(ctx, input, store)
// result.StopReason == "yielded"

result, err = g.Resume(ctx, result.RunID, loom.State{"approved": true}, store)
```

**Survive a crash.** Every step auto-checkpoints. Process dies after step C, restart, resume — it continues at step D with C's state intact:

```go
result, err = g.Resume(ctx, runID, loom.State{}, pgStore)
```

Newly written checkpoints carry `schema_version: 1`; newer binaries read legacy checkpoints, older binaries fail closed on newer ones. The same rule holds for `Resume`, `ResumeAt`, and history reads.

## Facts

- Five primitives — `State`, `Step`, `Router`, `Graph`, `Store`. No `Agent` class, no `Chain` abstraction, no `Memory` base type; everything else is composed, not inherited.
- The whole root package is ~1,650 lines across nine files (`wc -l`, comments included). Stdlib ~3,100; contract interfaces ~200.
- 306 tests and 4 fuzz targets; CI race-checks the CLI on Linux, macOS, and Windows with an 85% coverage gate.
- The core library has one runtime dependency (`google/uuid`). `pgx` is pulled only if you import `pgstore`.
- Benchmarks live in `tests/` — step overhead is in the microsecond range with the in-memory store. Run them yourself: `go test ./tests/ -run '^$' -bench=.`

## What the kernel deliberately doesn't know

| The kernel doesn't know | So you can |
|---|---|
| What an LLM is | Use OpenAI, Claude, DeepSeek, local models — swap freely inside a Step |
| What MCP is | Plug in any tool protocol — MCP / A2A / custom RPC |
| How to store memory | RAG, graph DB, full-text search — what goes in State is your call |
| How to serve HTTP | Gin, Echo, net/http — Loom is a library, not a service |

The kernel executes steps in graph order, checkpoints along the way, and pauses on yield. Everything else — prompts, tools, memory, transport — is your domain, assembled from `stdlib` building blocks (tool loop, permissions, budgets, sessions, sub-graphs) or your own code.

## When not to use Loom

- **You want batteries included.** Built-in RAG, vector memory, a visual flow designer, a hosted platform — Loom has none of these, on purpose. LangGraph or the OpenAI Agents SDK will get you there faster.
- **Your stack is Python or TypeScript.** Loom is Go only.
- **You need a control plane.** Multi-tenant auth, an approval UI, audit trails, addressable agents — that's [Weave](https://github.com/jinyitao123/Weave) (repo going public soon), the platform layer built on top of Loom. Loom itself stays a library plus a single-process CLI.

Note the layers differ: the *kernel* knows nothing about LLMs or memory; `stdlib` ships session and prompt utilities; the `loom` *CLI* adds MCP tools, session resume, and semantic memory on top.

## Comparison

| | Loom | LangGraph | OpenAI Agents SDK |
|---|---|---|---|
| Language | Go | Python | Python |
| Core library size¹ | ~1.6K LOC (root package, 9 files) | ~27.9K LOC (`libs/langgraph/langgraph`) | — |
| Persistence | Auto checkpoint per step | Checkpointer (opt-in) | None built in |
| LLM coupling | Zero | LangChain ecosystem | OpenAI-first |
| Tool protocol | Any | LangChain tools | function calling |
| Sub-graph nesting | Native | Native | Handoffs |
| Human-in-the-loop | yield / resume, state checkpointed | interrupt | Limited |

¹ `wc -l` on the core package, comments included, measured 2026-07-30: loom@`9f4c974`, langgraph@`4134145`. Same tool, same rules on both sides.

## The `loom` CLI

The repo also ships `loom`, a standalone agent engine built on the library — prompt JSON on stdin, one agent turn, NDJSON events on stdout, with MCP tool servers, session resume, semantic memory, and sub-agent orchestration compiled from an agent spec.

```bash
go install github.com/jinyitao123/loom/cmd/loom@latest

# or from a GitHub Release tarball (linux / macOS):
curl -fsSL https://raw.githubusercontent.com/jinyitao123/loom/main/install.sh | sh
```

See [cmd/loom/README.md](cmd/loom/README.md) for the event wire format, and [docs/host-integration.md](docs/host-integration.md) for driving it from any host process.

## Project structure

```
loom/
├── graph.go          Execution engine: State × Step × Router → Run / Resume
├── state.go          Typed map with registrable merge policies
├── step.go           type Step func(ctx, State) (State, error)
├── router.go         Control flow: Always / Branch / Condition
├── store.go          5-method persistence interface
├── lifecycle.go      Host observation seams (allocation / terminal / checkpoint)
├── options.go        GraphOption: merge / checkpoint / budget
├── memstore.go       In-memory Store (for testing)
│
├── contract/         Pure interfaces: LLM / ToolDispatcher / Embedder
├── stdlib/           Pre-built Steps & Hooks (tool loop, permissions, budget, session, …)
├── pgstore/          PostgreSQL Store
├── provider/         LLM providers (OpenAI-compatible / DeepSeek)
├── cmd/loom/         The `loom` CLI
├── tests/            Black-box test suite (public API only)
└── docs/             Host-integration contract & orchestration design
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). The short version: five primitives, layered imports, no business vocabulary in the kernel — and the CI guards to prove it.

## License

[MIT](LICENSE)
