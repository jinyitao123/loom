# Orchestration in `loom run`

Roadmap for compiling an `AgentSpec` into a `Graph` so a parent agent routes to
named sub-agents **deterministically**, instead of relying on the model to
"decide the next step" inside a flat tool loop.

## Motivation

The kernel already ships the orchestration primitives — `Graph` (composition of
`Step`s and `Router`s, with checkpointing/resume/budget) and `Router`
(`Always` / `End` / `Branch` / `Condition`). But `loom run` today executes a
single `NewToolLoopStep`: one agent, one loop. Cross-task routing, when needed,
falls back to prompting the model to pick — which is non-deterministic.

Goal: when a spec declares orchestration, `loom run` compiles it into a `Graph`
whose routing is a deterministic `Branch` on a state key. Same input → same
route, testable and reproducible.

## Spec surface

`stdlib.AgentSpec` carries two optional fields (`omitempty` → older specs parse
unchanged and stay a plain tool loop):

```go
SubAgents []SubAgentRef // declares orchestration
GraphType string        // "" / "standard" = default topology

type SubAgentRef struct {
    Name        string // sub-agent id
    Description string // when to route here (for the model)
    RouteKey    string // state value that routes here (defaults to Name)
}
```

## Compiler

`stdlib/compiler.CompileAgent(spec, llm, tools, opts) (*loom.Graph, error)`
builds the topology:

```
No sub-agents (default):
    prompt_assemble → chat → END
Orchestrator (sub_agents + a SubAgentResolver):
    prompt_assemble → chat ──[Branch on __delegate_to]──> sub_<name> → END
                                        └──(no match)──> END
```

The `chat` step's router is a deterministic `BranchFunc` on
`state["__delegate_to"]` (not a separate node): it maps that value (a sub-agent's
`RouteKey`, defaulting to its name, or the raw `sub_<name>` step name) to the
matching sub-graph step. Empty / no match → the graph ends (no delegation). What
*sets* `__delegate_to` is the `delegate` tool + chat-step wrapper described under
[How delegation fires](#how-delegation-fires).

### Lean by intent

The compiler is a deliberately lean, spec-driven build. It keeps the
orchestration topology + deterministic router + a tool-loop guard
(`loop_detector`, which breaks identical-call loops). It **excludes** several
things that belong to a host or were cut elsewhere, to keep the engine generic:

- memory retrieval / auto-remember — retrieval policy is a host concern (the
  host injects `recalled_memory` via stdin; see [host-integration.md](host-integration.md)).
- tracing / audit hooks, model-fallback routing, input guards, context
  compaction, embedder-based (semantic) skill matching.

These can be layered back as opt-in `CompileOpts` later without changing the
topology.

## How delegation fires

The router branches on `state["__delegate_to"]`, but a tool result cannot write
graph state — so something has to put the model's choice there. `loom run` does
it with a **model-callable `delegate(agent, task)` tool** (in `cmd/loom`):

1. A dispatcher wraps the real tools and adds a synthetic `delegate` tool whose
   `agent` enum is the declared sub-agents. When the model calls it, the choice
   is captured in a signal and a benign result is returned.
2. A chat-step wrapper (injected via `CompileOpts.ChatStepFactory`) runs the
   normal tool loop, then — if `delegate` fired — returns
   `state{__delegate_to, last_user_message}` so the compiler's deterministic
   `Branch` routes to `sub_<agent>`; otherwise it returns the model's own answer.

This needs **no new NDJSON event** — the `delegate` call surfaces through the
existing `tool_use`/`tool_result` events, and routing rides existing graph state
+ the `output` key. The cross-repo wire contract is untouched.

## Status — implemented

- ✅ `AgentSpec.SubAgents` + `GraphType` (`stdlib/specloader.go`).
- ✅ `stdlib/compiler` — `CompileAgent`, deterministic router, `loop_detector`,
  optional `ChatStepFactory` hook (nil = default tool-loop build).
- ✅ Wired into `loom run` (`cmd/loom`): `runAgent` forks — orchestration declared
  (`len(SubAgents) > 0`) → `CompileAgent` → `Graph.Run`; else the original single
  tool-loop path, byte-for-byte (zero regression). `recalled_memory` + `notice`
  reach the graph via `CompileOpts.Context`; the streaming LLM + tool-event hooks
  pass through so child tokens/tool calls stream. Sub-agents resolve from
  `<cwd>/.loom-agents/<name>.json` as leaf graphs (single-hop, no recursion).
- ✅ Verified end-to-end against a real model: a two-sub-agent orchestrator routes
  selectively (each request reaches the matching sub-agent, which applies its own
  identity), stream carries only the known event types.

`go build ./... && go vet ./... && go test ./...` green.

## Roadmap

### Sub-agent observability (optional, additive)

Deterministic routing works over the existing event stream — sub-agent output
flows through the same `text` / `tool_*` events. Surfacing *which* sub-agent ran
(enter / route / exit events) is an **additive** stream-json change. Because the
NDJSON shapes are a wire contract (see host-integration.md), any new event must
land in lockstep with downstream consumers' contract tests — treat it as its own
change, not folded into the wiring above.

### Verification

An orchestrator spec routes the same input to the same sub-agent every run; a
spec without sub-agents still runs as a plain tool loop. Exercise end-to-end
against a real model.
