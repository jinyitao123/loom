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
    prompt_assemble → chat → route → sub_<name> → END
```

`route` is a deterministic `BranchFunc`: the chat step (or a tool) writes
`__delegate_to` into state; the router maps that value (or a sub-agent's
`RouteKey`, or its raw `sub_<name>` step name) to the matching step. No match →
the graph ends (no delegation).

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

## Status

- ✅ `AgentSpec.SubAgents` + `GraphType` (`stdlib/specloader.go`).
- ✅ `stdlib/compiler` — `CompileAgent`, deterministic router, `loop_detector`.
  Unit tests: standard vs orchestrator topology, resolver-less sub-agents stay
  standard, nil-spec / resolver-error paths, router (RouteKey / step name /
  no-match → halt), loop detector. `go build ./... && go test ./...` green.

## Roadmap

### Wire into `loom run`

`runAgent` chooses the execution model: orchestration declared
(`GraphType != "" || len(SubAgents) > 0`) → `CompileAgent` → `Graph.Run`; else
the current `NewToolLoopStep` (zero regression for non-orchestrating agents).

Two integration points to get right:

1. **Prompt composition.** The compiled graph owns its `prompt_assemble` step,
   so the run path must not pre-assemble. Externally injected context
   (`recalled_memory`, `notice`) should reach the graph via `CompileOpts.Context`
   (surfaced by the prompt assembler) rather than a pre-set `__system_prompt`
   the assembler would overwrite.
2. **`SubAgentResolver`.** Resolves a sub-agent name → a compiled child graph.
   The minimal first form builds a single-purpose child from the `SubAgentRef`
   (its description as identity); richer forms can resolve a nested child spec.

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
