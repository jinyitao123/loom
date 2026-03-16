<p align="center">
  <img src="assets/loom-social-preview.png" alt="LOOM · 织机" width="800">
</p>

<p align="center">
  <a href="README.md">English</a> | <a href="README.zh-CN.md">中文</a>
</p>

---

A loom has only three moving parts — warp, weft, shuttle — yet it can weave any pattern.

Loom works the same way. The entire kernel is 584 lines of Go and 5 type definitions. Combined, they express everything from a single chatbot to a hundred-agent orchestration.

```go
type State  map[string]any                                           // data
type Step   func(ctx context.Context, state State) (State, error)    // compute
type Router func(ctx context.Context, state State) (string, error)   // control flow
type Store  interface { Get; Put; Delete; List; Tx }                 // persistence
type Graph  struct { steps; routers; Run(); Resume() }               // orchestration
```

No `Agent` class. No `Chain` abstraction. No `Memory` base type.

Every advanced feature is **composed** from these five primitives — not **inherited** from a framework.

## Why Loom

There is no shortage of agent frameworks. What's missing is one you can actually **own**.

Most frameworks are feature-complete — tens of thousands of lines, rich abstraction layers, batteries included. But when you need to change their behavior, understand their internals, or embed them into your own system, you find yourself wrestling a giant.

Loom's design principle is the inverse: **the kernel is small enough to read in an afternoon.** Not because it does less, but because a mature, complex system must have a lean core. Complexity should emerge from composition, not be pre-baked into the framework.

## 30-Second Quickstart

```go
package main

import (
    "context"
    "fmt"
    "github.com/jinyitao123/loom"
)

func main() {
    greet := func(_ context.Context, s loom.State) (loom.State, error) {
        return loom.State{"output": "Hello, " + s["name"].(string) + "!"}, nil
    }

    g := loom.NewGraph("greeter", "greet")
    g.AddStep("greet", greet, loom.End())

    result, _ := g.Run(context.Background(), loom.State{"name": "World"}, nil)
    fmt.Println(result.State["output"]) // Hello, World!
}
```

```bash
go get github.com/jinyitao123/loom
```

## What Five Primitives Can Do

<table>
<tr>
<td width="50%">

**Tool-calling Agent**

Three Steps, wired together.

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

**Pause for Human Approval**

A Step returns `__yield: true` and the Graph freezes state automatically. After approval, `Resume()` picks up right where it left off.

</td>
<td>

```go
result, _ := g.Run(ctx, input, store)
// result.StopReason == "yielded"

// After human approval
result, _ = g.Resume(ctx, result.RunID,
    State{"approved": true}, store)
```

</td>
</tr>
<tr>
<td>

**10 Agents Collaborating**

Each agent is a Graph, nested inside a parent Graph. Shared checkpoints, shared step budget.

</td>
<td>

```go
parent := loom.NewGraph("orchestrator", "dispatch",
    WithStepBudget(500))

parent.AddStep("dispatch", router, Branch(...))
parent.AddStep("analyst", SubGraphStep(analystGraph), ...)
parent.AddStep("coder",   SubGraphStep(coderGraph),   ...)
```

</td>
</tr>
<tr>
<td>

**Process Crashed?**

Nothing to do. Every step auto-checkpoints to PostgreSQL. After restart, `Resume()` continues from the last checkpoint. Not a single step lost.

</td>
<td>

```go
// Before crash: A → B → C ✓ → [crash]
// After restart:
result, _ = g.Resume(ctx, runID, State{}, pgStore)
// Continues from D, C's state fully preserved
```

</td>
</tr>
</table>

## What the Kernel Deliberately Doesn't Know

This is Loom's most important design decision.

| The kernel doesn't know | So you can |
|---|---|
| What an LLM is | Use OpenAI, Claude, DeepSeek, local models — swap freely inside a Step |
| What MCP is | Plug in any tool protocol — MCP / A2A / custom RPC |
| How to store memory | RAG, graph DB, full-text search — what goes in State is your call |
| How to serve HTTP | Gin, Echo, net/http — Loom is a library, not a service |

The kernel does one thing: **execute Steps in the order defined by the Graph, checkpoint along the way, pause on yield.**

Everything else is your domain. That's freedom, not omission.

## Architecture

```
┌──────────────────────────────────────────────────┐
│  Layer 3 · Your App                              │  ← HTTP / Auth / Multi-tenancy / Your business
├──────────────────────────────────────────────────┤
│  Layer 2 · Stdlib                    ~1800 LOC   │  ← Building blocks: ToolLoop / Guard / Handoff
├──────────────────────────────────────────────────┤
│  Layer 1 · Contract                   ~300 LOC   │  ← Pure interfaces: LLM / ToolDispatcher / Embedder
├──────────────────────────────────────────────────┤
│  Layer 0 · Kernel                     ~600 LOC   │  ← Five primitives. That's it.
└──────────────────────────────────────────────────┘
```

**Dependency rule: Layer N may only import Layer N-1 or below. No exceptions.**

## Stdlib

Every component in the standard library is a composition of Steps or Routers. No new primitives, no special channels.

```go
// ToolLoop: LLM call → tool execution → result → loop until done
chat := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
    MaxIterations: 20,
    Compaction:    &compactionPolicy,
    ToolHooks:     []contract.ToolHook{auditHook},
})

// Declarative tool permissions: deny takes precedence over allow
safeTool := stdlib.NewPermissionDispatcher(tools,
    []string{"rm_rf", "drop_table"},   // always blocked
    []string{"read_*", "search_*"},    // only these allowed
)

// Auto-stop at $5
g.SetHooks(loom.HookPoints{
    After: []loom.StepHook{stdlib.CostBudgetHook(5.00)},
})
```

Read-only tools run in parallel automatically; stateful tools run serially. ToolLoop reads `ToolDef.ReadOnly` to decide.

## Project Structure

```
loom/
├── graph.go          Execution engine: State × Step × Router → Run / Resume
├── state.go          Typed map with registrable merge policies
├── step.go           type Step func(ctx, State) (State, error)
├── router.go         Control flow: Always / Branch / Condition
├── store.go          5-method persistence interface
├── options.go        GraphOption: merge / checkpoint / budget
├── memstore.go       In-memory Store (for testing)
│
├── contract/         Pure interfaces: LLM / ToolDispatcher / Embedder
├── stdlib/           Pre-built Steps & Hooks
│   ├── toolloop.go   LLM ↔ Tool loop
│   ├── steps.go      Guard / HumanWait / SubGraph / Handoff
│   ├── permission.go Declarative tool permissions
│   ├── budget.go     Token & USD budget hooks
│   ├── prompt.go     Tiered prompt assembly
│   └── session.go    Session history persistence
│
├── pgstore/          PostgreSQL Store
└── provider/         LLM Providers (OpenAI / DeepSeek)
```

## Comparison

| | Loom | LangGraph | OpenAI Agents SDK |
|---|---|---|---|
| Language | Go | Python | Python |
| Kernel | ~600 LOC | ~15K LOC | ~3K LOC |
| Persistence | Auto checkpoint | Auto checkpoint | None |
| LLM coupling | Zero | Medium | Strong (OpenAI-bound) |
| Tool protocol | Any | LangChain Tools | function calling |
| Sub-graph nesting | Native | Native | Not supported |
| Human-in-the-loop | yield / resume | interrupt | Limited |
| Embeddable | Yes (Go package) | No (Python service) | No (Python service) |

## Who Is This For

- Long-running agents that need **crash recovery**
- Enterprise workflows that need **human-in-the-loop** approval
- **Multi-agent orchestration** without a heavyweight framework
- **Budget control** (token / USD) to prevent runaway agents
- Embedding agent capabilities in the **Go ecosystem**

## License

Apache-2.0

---

<p align="center">
  <sub>A mature, complex product must have a lean, precise kernel.</sub>
</p>
