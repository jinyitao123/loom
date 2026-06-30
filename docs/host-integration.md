# Host Integration Contract

How an external host process drives `loom run` as an embedded agent engine.

Loom is a library first (`Graph`/`State`/`Step`/`Router`/`Store`) and a CLI
second. `loom run` exposes the engine to any host over three stable surfaces:
**process flags**, a **stdin JSON payload**, and a **stdout NDJSON stream**. A
host that speaks these three can run a stateful agent without linking Go.

This document is the contract. It is engine-side only — it says nothing about
any particular host's storage, scheduling, or business domain.

## Why a host embeds loom

A frontier chat CLI is stateless: one prompt in, one answer out. An agent that
must *remember*, stay *in presence* across turns, and *route* work
deterministically needs a stateful kernel — exactly loom's `Graph`/`State`/
`Store`. A host delegates those agents to `loom run` and keeps its own concerns
(identity storage, scheduling, transport, tools) outside the engine.

## The three surfaces

### 1. Process invocation

```
loom run \
  --cwd <agent-workdir> \
  --model <model-id> \
  [--format stream-json|text] \
  [--resume <session-id>] \
  [--agent-config <path>]   # default: <cwd>/.loom-agent.json
  [--mcp-config <path>]     # default: <cwd>/.loom-mcp.json
  [--profile <name>]
  [--bypass-permissions]
```

The host prepares a per-agent working directory, writes the agent's spec into
it (below), and spawns one process per task. Provider credentials come from the
environment (e.g. `DEEPSEEK_API_KEY`, `OPENAI_API_KEY`, or `LOOM_BASE_URL` +
`LOOM_API_KEY` for any OpenAI-compatible endpoint).

### 2. stdin — the task payload

A single JSON object on stdin. Only the modeled fields are consumed; unknown
fields are ignored (forward-compatible).

```json
{
  "type": "user_message",
  "instruction": "the task / user turn",
  "notice": "optional out-of-band note folded into the system prompt",
  "recalled_memory": ["fact the host recalled for this task", "..."]
}
```

- `instruction` (required) — the turn to act on. A bare non-JSON stdin is also
  accepted and treated as the instruction (human testing).
- `recalled_memory` — **memory is the host's concern, not the engine's.** The
  host decides what past context is relevant and passes it here; loom folds it
  into the system prompt. Loom does not run its own vector store or
  auto-remember in `run` — keeping retrieval policy, scope, and storage on the
  host side, where the host already owns identity and tenancy.

### 3. `.loom-agent.json` — the agent spec

Read from the working directory (`stdlib.AgentSpec`). The host compiles its own
agent definition into this file; the engine never reads the host's database.

```jsonc
{
  "identity": { "core": "always-in-prompt instructions",
                "extended": "loaded when budget allows" },
  "profiles": { "<name>": { "system_addition": "...", "greeting": "..." } },
  "skills":   [ { "name": "...", "description": "...", "body": "...",
                  "always_active": false } ],
  "sub_agents": [ { "name": "...", "description": "when to route here",
                    "route_key": "state value that routes here" } ],
  "graph_type": ""
}
```

Notes:
- `identity.core` is always in the prompt; `extended` is tiered in under budget.
- **Skill bodies are inlined.** Loom injects `SkillDef.body` directly (matched by
  name/description) — there is no on-demand file read, so the host must inline
  the body, not a path.
- `sub_agents` / `graph_type` drive deterministic orchestration — see
  [orchestration.md](orchestration.md). Both are optional; absent, the run is a
  plain tool loop (back-compat).

### 4. stdout — the NDJSON event stream

With `--format stream-json`, the engine emits one JSON event per line:
`session_init`, `text`, `thinking`, `tool_use`, `tool_result`, `turn_end`,
`error`, `result`. A host parses these line-by-line to stream output and detect
completion (`result` carries final status + session id).

> **Contract stability.** The NDJSON event shapes are a wire contract. A host
> typically pins them with its own contract test. Any **new** event type or
> field must be added in lockstep on both sides — engine emitter and host
> parser — or a host on the old shape breaks. Add fields additively; never
> repurpose an existing one.

## Session continuity

`--resume <session-id>` reloads prior turns from the session store
(`.loom-sessions/` by default) so an agent continues a conversation across
process invocations. This is how a per-turn CLI sustains a multi-turn,
in-presence agent.

## What stays out of the engine

By design, the following are **host** responsibilities, deliberately not baked
into `loom run`: memory retrieval/policy, identity & tenancy, scheduling,
transport/realtime, and tool provisioning (beyond MCP config). The engine stays
a generic kernel; domain and platform specifics live in the host.
