# Provider-recorded recovery fixtures

`stdlib/testdata/provider_recordings/deepseek-flash.json` contains real responses
recorded from the official DeepSeek Chat Completions endpoint on 2026-10-02.
The prompt, two lookup inputs, and sum are synthetic. No application records,
employee content, credentials, Authorization headers, or provider reasoning
text are stored. This is a new protocol test, not an export of any historical
application failure.

The three normal turns returned HTTP 200 with tool-call batches of 2, 1, and 0.
They use the existing non-thinking tool profile: `thinking.type=disabled` and
`reasoning_effort=none`. Tests replay these actual wire envelopes through the
OpenAI-compatible adapter on a loopback server. They do not call a live provider
or require an API key.

## What the recovery matrix checks

The same durable sequence-journal harness covers 48 cases: six operation
boundaries, four crash positions, and both uninterrupted-budget and controlled
round-budget execution. It verifies provider-accepted message history, paired
assistant calls and tool results, unchanged final output, exactly one execution
of each pure tool, and saved quota pauses followed by authorized continuation.
An unknown tool outcome remains fenced through three retries until the host
reconciles it. Dropping a recorded tool result is rejected independently of the
loop; incorrectly settling an already-performed operation as unperformed makes
the execution counter violate the invariant. These mutations do not manufacture
an upstream HTTP failure.

The test host binds each journal segment to its frozen slice-entry checkpoint.
A failed attempt may add a newer error checkpoint; using that volatile sequence
as a new segment would skip unresolved operations and can repeat an operation.
The recording exposed this test-host error when pausing after every model round.
The fix is confined to the host harness. Kernel, contract, provider, CLI, and
default execution behavior are unchanged.

## Thinking-mode observation and limits

The capture also made a thinking-mode request with actual nonempty
`reasoning_content` and two real tool calls. It saved only a redaction placeholder,
byte length, and SHA-256 for continuation fields. A subsequent request preserving
the original continuation in memory returned HTTP 200. The new synthetic request
omitting it also returned HTTP 200; this observation is preserved rather than
replaced by an expected 400.

[DeepSeek's thinking documentation](https://api-docs.deepseek.com/guides/thinking_mode/)
requires continuation to be returned when tools are present. This recording does
not establish why an earlier, unavailable request failed, or prove that omission
is generally supported. The current Loom contract does not carry that field and
the existing tool profile deliberately disables thinking. This work does not
implement or claim thinking-continuation recovery. The redacted thinking capture
cannot be used as a byte-for-byte live replay of that private continuation.

No OpenAI API key was available. OAuth/subscription credentials were excluded.
There is no genuine OpenAI recording in this change, so the two-provider recovery
requirement remains incomplete. The use of `provider/openai` is a protocol
adapter, not a claim that DeepSeek responses came from OpenAI. The tool-message
pairing guidance is also documented in [OpenAI function calling](https://developers.openai.com/api/docs/guides/function-calling).

## Recording and verification

Recording is explicit and bounded; it is never part of CI:

```sh
python3 scripts/record-provider-recovery.py \
  --provider deepseek --model deepseek-flash \
  --auth-file "$HOME/.pi/agent/auth.json" --thinking-probe \
  --output stdlib/testdata/provider_recordings/deepseek-flash.json
```

Alternatively use the provider's API-key environment variable. The script
accepts API keys only, uses official HTTPS endpoints, restricts tools and
arguments to the synthetic task, and emits status/count metadata only. It records
raw request/response digests, with reasoning fields redacted before writing.
OpenAI recording requires an explicit authorized model and `OPENAI_API_KEY`;
it does not reuse OAuth or add DeepSeek-specific request fields to OpenAI.

```sh
go test ./stdlib -run 'TestRecovery|TestJournal' -count=1
go test -race ./stdlib -run 'TestRecovery|TestJournal' -count=1
```

The recovery CI already runs stdlib tests on Linux, macOS, and Windows and the
recovery/journal subset under Linux race detection. Local checks do not imply a
new remote CI run, publication, or any host application's business acceptance.
