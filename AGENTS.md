# AGENTS.md

Guidance for AI coding agents (and humans) working in this repository. For what
Loom *is* and how to use it, read `README.md` first — this file is about how to
change the code without breaking its design.

Loom is a minimal agent kernel: five primitives (`State` · `Step` · `Router` ·
`Graph` · `Store`) with a thin stdlib and a CLI. Keep it minimal.

## The one rule that governs everything

**Layered dependency: Layer N may import only Layer N-1 or below. No exceptions.**

```
Layer 3  your app / host          (not in this repo)
Layer 2  stdlib/                  building blocks — compositions of Steps/Routers
Layer 1  contract/               pure interfaces: LLM / ToolDispatcher / Embedder
Layer 0  kernel (root *.go)      the five primitives
```

Concretely:
- The kernel (`graph.go`, `state.go`, `step.go`, `router.go`, `store.go`,
  `options.go`, `memstore.go`) imports **nothing from `contract/`, `stdlib/`,
  `provider/`, `pgstore/`, or `cmd/`**. It is self-contained.
- `contract/` is pure interfaces + value types. No behavior.
- `stdlib/` composes the kernel + contract. **Do not add new primitives here** —
  everything is a `Step` or a `Router`. If you're tempted to add a new channel,
  event type, or control-flow mechanism, express it with the existing five.
- `provider/` and `pgstore/` are leaf adapters implementing `contract` interfaces.
- `cmd/loom/` is the CLI (Layer 3-ish): it wires the engine to stdio/env.

If a change needs to break the layering, it's the wrong change.

## Build / test / lint (match CI before you commit)

CI (`.github/workflows/loom-cli.yml`) gates on `cmd/loom`; run these locally:

```
go build ./...
go test ./...                 # full suite; CI also runs -race on ./cmd/loom/
go test -race ./cmd/loom/     # the CLI, race-checked (what CI runs)
gofmt -l .                    # MUST print nothing (CI fails on any output)
go vet ./...
golangci-lint run             # config in .golangci.yml
```

Go 1.24, module `github.com/jinyitao123/loom`. New code needs tests; the suite
is table-driven and fast — mirror the nearest `_test.go`. There are fuzz targets
in `cmd/loom` (`go test ./cmd/loom/ -run '^$' -fuzz Fuzz... -fuzztime 20s`).

## Repo map

`README.md` has the full tree. Quick orientation:
- **root `*.go`** — the kernel. Change with extreme care; everything depends on it.
- **`contract/`** — interfaces. Adding a method here ripples to every provider.
- **`stdlib/`** — `toolloop.go`, `steps.go` (Guard/HumanWait/SubGraph),
  `prompt.go` (tiered assembly), `permission.go`, `budget.go`, `session.go`,
  `specloader.go` (the `.loom-agent.json` `AgentSpec`), `compiler/` (spec →
  runnable Graph for orchestration).
- **`provider/`** — OpenAI + DeepSeek LLM adapters.
- **`pgstore/`** — Postgres `Store`.
- **`cmd/loom/`** — the `loom run` CLI.
- **`docs/`** — `host-integration.md` (how a host drives `loom run`),
  `orchestration.md` (sub-agent routing).

## Invariants worth knowing before you touch these areas

- **The CLI wire contract is stable.** `loom run --format stream-json` emits a
  fixed set of NDJSON events (`session_init` / `text` / `thinking` / `tool_use`
  / `tool_result` / `turn_end` / `error` / `result`). Hosts pin these. Add event
  types/fields **additively** and never repurpose an existing one — a new event
  must land in lockstep with any consumer's contract test. See
  `docs/host-integration.md`.
- **Skill bodies are inlined.** Loom injects `SkillDef.Body` directly (matched on
  name/description); there is no on-demand file read. A spec must carry the body,
  not a path.
- **Orchestration is single-hop + opt-in.** `AgentSpec.SubAgents`/`GraphType` are
  optional (`omitempty`); absent → a plain tool loop (unchanged). Sub-agents are
  compiled as leaf graphs (no nested `sub_agents`). Routing is deterministic: the
  model calls a `delegate` tool → `state["__delegate_to"]` → the compiler's
  `Branch` fires. See `docs/orchestration.md`.
- **`Date.now`-style nondeterminism** in the kernel breaks `Resume`. Thread
  timestamps/IDs through `State` (e.g. `__run_id`) rather than reading the clock
  mid-graph.

## Keep it an engine

Loom is a generic kernel. Do **not** bake host, transport, storage-policy,
identity/tenancy, or any domain/business specifics into it — those belong in the
Layer-3 host that embeds `loom run`. Examples in docs/comments stay generic.

## Docs

When you change the CLI contract or orchestration behavior, update the relevant
file under `docs/` in the same change. Keep docs engine-perspective — no
host-specific or domain content.
