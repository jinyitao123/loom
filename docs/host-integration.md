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

## Optional provider protocol observations

Embedded Go hosts may opt a single provider invocation into
`openai.WithProtocolObserver(ctx, options, callback)`. No observer is installed
by default. This context option does not change `ChatRequest`, model request
JSON, `ChatResponse`, `StreamChunk`, CLI events or journal response formats.
Place an observing wrapper inside `stdlib.NewJournaledLLM` when replay must not
collect another observation. The host owns authorization, expiry, attempt
identity, quotas and persistence; the provider adds no storage or execution loop.

The callback receives one `contract.ProtocolObservation` per `Client.Chat` or
`Client.Stream` invocation, including pre-send refusals and failed HTTP attempts.
For streaming it runs before the returned channel closes; for Chat it runs
before the method returns. Callbacks must be short and synchronize shared state.
A nil callback disables collection, and a callback panic is isolated from model
execution. A caller that cancels must still drain the stream to closure.

The value contains only tool-definition count/SHA-256, bounded data-frame and
parse-failure counts, tool-delta indexes, assembled/emitted call counts, argument
byte lengths/SHA-256, a finite finish reason, terminal shape and completeness.
It never contains messages, response text, raw frames, headers, API keys, tool
names, call IDs, argument text or reasoning. The definition digest is SHA-256 of
the actual serialized OpenAI `tools` value (`null` represents an omitted list).
`RequestSent` requires the HTTP trace's successful `WroteRequest` boundary;
pre-send/transport failures cannot prove the provider received the request.

`RequestOptions` records the enum values actually serialized for `thinking`,
`reasoning_effort`, `response_format` and `tool_choice` (empty when omitted,
`other` for an unrecognized value); it is absent when the request was refused
before its wire form existed. `Content` counts response text bytes, DeepSeek-style
text tool-call markers (`｜DSML｜`) found in ordinary content, and reasoning
frames/bytes. Marker text is not an executable call, so a zero-call response
with markers indicates a provider-side text call carrier rather than a prose
answer. Only counts are kept; no content or reasoning text is retained.

Absence means not collected or unsupported. `Complete=false` means the counts
cannot establish zero calls. Malformed/unsupported frames, sparse/negative indexes, missing
tool identities, unknown/missing finish reasons, EOF, cancellation, scan errors
and capture limits remain incomplete even when some counts are available. A
tool-call finish without any assembled call is also incomplete.
`DoneSeen` records the actual SSE sentinel, independently of completeness.
Normal text with an observed finish and DONE can prove a complete zero-call
protocol response, which is not proof that a requested task succeeded.

The supported SSE delta and non-streaming message fields are `content`, `tool_calls`, `role`,
`reasoning_content` and `refusal`. The last three do not carry tool calls and
are never copied into observations. A non-null `function_call` or other unknown
delta/message field makes the observation incomplete: the adapter does not execute that
unsupported protocol, and cannot call its ignored value a complete zero-call
response. Null compatibility fields do not contain a call. This validation only
affects observation completeness; legacy execution and default wire behavior
remain unchanged.

`MaxDataFrames`, `MaxToolDeltas` and `MaxToolIndexes` bound metadata only. Defaults
are 16,384 / 4,096 / 128; hard ceilings are 65,536 / 65,536 / 256. Exceeding a
limit sets `Truncated` and never truncates model output or tool execution.
Argument indexes are wire indexes for SSE and array positions for Chat.
Sparse SSE indexes are assembled in sorted index order so they cannot silently
lose a call; the observation still reports that protocol shape as incomplete.
Malformed-frame skipping retains its existing behavior, but can no longer look
complete to an enabled observer. These are synthetic protocol tests and future
observations, not reconstructed evidence for an earlier unobserved request.

## Embedded lifecycle hooks

An embedded Go host that needs durable run admission or terminalization can
use per-invocation `LifecycleHooks`. Hooks are not stored on `Graph`, so the
same graph remains safe for concurrent callers with different host policies.

- `RunWithLifecycle` requires a caller-supplied `__run_id`.
- `ResumeWithLifecycle` retains the existing run identity.
- `ResumeAtWithLifecycle` requires an explicit new run ID and calls
  `RunAllocated` after restoring and merging the source checkpoint but before
  any step, router, or no-step return.
- `ExecutionContext` optionally derives the router/step context after
  `RunAllocated` succeeds, for example to attach a host-owned lease or fencing
  cancellation scope. It must return a non-nil context derived from the
  supplied invocation context.
- `Terminalizer` runs synchronously exactly once before every non-nil
  `RunResult` is returned. Its error is joined with any graph error.
- `CheckpointObserver` exposes only the required/latest store boundaries
  `latest_put_before` and `latest_put_after`; history writes remain outside the
  contract.
- `stdlib.SubGraphOpts.LifecycleHooks` applies the same contract to fresh and
  resumed child graphs.

The event types contain only engine facts. Hosts remain responsible for
storage transactions, fencing, retries, and domain-specific records.

## Resuming after a graph step budget stop

`WithStepBudget` limits graph steps across a parent and its subgraphs. When
the budget rejects the next step before execution, the graph returns
`StopBudget` and an error matching `errors.Is(err, loom.ErrBudgetExhausted)`.
The rejected step consumes no budget. With a store, the graph checkpoints
that unexecuted step with `mid_step` phase and the remaining balance. For
example, if a budget of one allows `a` in `a → b`, the new checkpoint points
to `b`. After the host explicitly provides a positive `__budget_remaining`
through resume input, `Resume` continues at `b`; `a` is not repeated.
`ResumeAt` from this checkpoint uses the same entry position in a new run.
Resuming with no additional balance stops again before executing a step.

Checkpoint policy still applies. A failed required checkpoint returns
`StopError`; its error retains both the budget sentinel and the persistence
error. The default best-effort policy logs a failed store write and returns
`StopBudget`, leaving the previous stored checkpoint in place. That older
checkpoint does not prove the new continuation position was saved. A nil
store performs no persistence. Hosts requiring reliable continuation must
provide a store and use `CheckpointRequired`; lifecycle terminalization also
reports `LatestCheckpointPersisted`.

This changes graph-level budget stops only. Ordinary step errors, ToolLoop
iteration limits, CLI events, and legacy checkpoints with an empty phase
retain their existing behavior.

## What stays out of the engine

By design, the following are **host** responsibilities, deliberately not baked
into `loom run`: memory retrieval/policy, identity & tenancy, scheduling,
transport/realtime, and tool provisioning (beyond MCP config). The engine stays
a generic kernel; domain and platform specifics live in the host.
