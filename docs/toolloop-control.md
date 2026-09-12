# Controlled ToolLoop pauses

`ToolLoopOpts.Control` opts into persistent model-round slices. With `Control == nil`, the existing tool loop, park behavior, and final wrap-up call are unchanged.

```go
step := stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
    MaxIterations: 20,
    Control: &stdlib.ToolLoopControl{
        ID: "chat",
        InitialTotalRounds: 60,
    },
})
graph := loom.NewGraph("controlled", "chat",
    loom.WithCheckpointPolicy(loom.CheckpointRequired),
)
graph.AddStep("chat", step, loom.End())
```

Run this graph with a persistent `Store`. Required checkpoint policy alone does not supply a store. The loop remains one `Step`; graph step budgets therefore cannot count its internal model rounds. `MaxIterations` bounds each slice, and `InitialTotalRounds` bounds the cumulative main-model rounds. A final response, a repeated batch blocked before dispatch, and a provider-truncated response each consume one round. Compaction model calls belong in the host's usage ledger and do not increase the main-loop round counter. Controlled budget pauses make no extra wrap-up call.

The step returns a private snapshot and `nil` error with the existing `mid_step` yield protocol. Graph merges the delta, runs After hooks, saves its checkpoint, and returns `StopYielded`. Always inspect Graph's returned error first: `ReadToolLoopOutcome` describes a state and does not prove that it was saved successfully.

| Outcome reason | Meaning |
|---|---|
| `final_response` | The model returned normally without tool calls |
| `tool_stop` | A validated tool result requested `StopLoop` |
| `slice_limit` | This slice is exhausted, with cumulative rounds remaining |
| `total_limit` | The cumulative round authorization is exhausted |
| `repeat_limit` | A repeated batch was stopped before dispatch |
| `provider_length` | A `length` response was saved without executing its calls |
| `provider_stop` | Another non-normal provider stop reason needs host handling |
| `await_tool_result` | Existing park protocol is waiting for real tool results |

Use `ReadToolLoopOutcome(result.State)` for typed classification. Missing control data returns `present=false`; corrupt protocol data returns an error. A valid active snapshot has `present=true` and an empty reason. A loop outcome does not establish any application acceptance result.

Normal completion uses the existing `SetOutput` and validated StatePatch/StateOps staging path. A pause retains staged results privately and leaves public state changes uncommitted. The private message snapshot is restored as a whole; it is never append-merged into public `messages`. Completed checkpoints use `after_step` so ordinary Resume does not repeat the completed loop.

## Host-authorized continuation

`PrepareToolLoopResume` accepts only `slice_limit` or `total_limit`, with no pending tools. Supply a nonempty grant ID, the exact run ID, checkpoint sequence, yield token and slice, and an absolute authorized total. The total cannot decrease or be at or below the rounds already used. The result is a private delta for `Graph.Resume`; it preserves cumulative usage, message history, staged changes and repetition counters, while starting one new slice.

The helper is a pure transformation of the supplied snapshot. It does not read Store, consume grants or serialize competing requests. The host must retain immutable grant receipts, reject one grant ID with different contents, and atomically compare the source checkpoint before any new effect. Reusing an old snapshot can reproduce an old valid delta, and directly replaying that delta into bare Graph.Resume can overwrite newer state. **Graph.Resume is not a substitute for host checkpoint CAS and grant deduplication.**

A bare Resume without a grant keeps a controlled limit pause and performs no model/tool calls. Normal host polling should read the saved pause instead of repeatedly asking Graph to save another equivalent pause. Park resume accepts the existing real `__resumed_tool_results`, preserves already consumed rounds, and does not grant a new slice. If the last slice round parked, supplying its result reaches the limit immediately without an extra model call.

The control record binds one `__run_id` and configured loop ID. Recreating the step does not reset its counters. Applying a changed model request policy or copying a controlled snapshot into a different run is rejected. The policy hash covers fixed request options and round limits; callback and tool implementation identity remain a host configuration responsibility.

When reached through `NewSubGraphStep` or `NewHandoffStep`, a controlled ToolLoop rejects at its own entry before making its own model/tool calls. Earlier child steps may already have run, including a legacy ToolLoop's model/tool effects; this guard cannot prevent or roll them back. The child continuation protocol has not been extended for round grants.

The host must reject unsupported compositions before execution using its known configuration and capability checks. Graph's opaque Step functions do not provide a proof of the full topology's capabilities. The stdlib entry guard is a local defense, not whole-graph admission.

## Hooks and crash recovery

After hooks run before Graph checks yield. Hosts must keep safety checks while skipping completion-only effects for valid controlled pauses. Do not use ordinary `delta + error` to express a controlled pause: the existing Graph error path does not merge that delta.

The private snapshot supports continuation from a saved pause. It does not provide exactly-once effects between checkpoints. A host that journals individual model/tool operations must replay a crashed slice from its unchanged entry state and original journal segment. A newly authorized slice starts from the newer message/counter snapshot and needs a new journal segment, atomically saved with grant consumption before effects. Reusing the old segment with cursor zero would mismatch its original model request or repeat accounting.

The first supported topology is a serial leaf graph. Nested controlled continuation, cross-run forks, CLI wire integration and application-level authorization remain separate adapters. No kernel primitive, routing rule or Step signature changes are required.
