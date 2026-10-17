# Interactive agent (`loom agent`)

`loom agent --mode rpc` runs a long-lived agent that a host application drives
over stdio: one JSON command per input line, one JSON response or event per
output line. Where `loom run` executes a single turn and exits, `loom agent`
keeps a session open, accepts one prompt after another, persists every message
to a session file and can continue that file after a restart.

The command/event vocabulary and the session file follow the widely used
coding-agent JSONL conventions (session format version 3), so a host written
for such agents can drive Loom without a separate integration.

Code: `interactive/` (session file, agent, protocol server), `localtools/`
(local file and shell tools), `cmd/loom/agent.go` (flags and wiring). The
agent runs each prompt through `stdlib.NewToolLoopStep`; events come from a
model wrapper and tool hooks, so there is no second agent loop.

## Starting

```sh
loom agent --mode rpc [--cwd DIR] [--session FILE] [--session-dir DIR] \
           [--provider NAME] [--model ID] [--thinking LEVEL] [--tools read,bash,edit,write]
```

| Setting | Default |
| --- | --- |
| Configuration directory | `$LOOM_AGENT_DIR`, else `~/.loom/agent` |
| Session root | `$LOOM_SESSION_DIR`, else `<configuration>/sessions` |
| New sessions | `<session root>/--<cwd with / \ : replaced by ->--/<timestamp>_<uuid>.jsonl` |
| Tools | `read,bash,edit,write`; `grep,find,ls` are also available |
| Model | `--provider/--model`, else the session's last model, else the first configured model |

Models come from `<configuration>/models.json`:

```json
{"providers": {"local": {"baseUrl": "http://127.0.0.1:11434/v1", "api": "openai-completions",
  "apiKey": "LOCAL_KEY", "models": [{"id": "qwen2.5-coder", "reasoning": false, "contextWindow": 128000}]}}}
```

API keys may also be stored in `<configuration>/auth.json` as
`{"<provider>": {"type": "api_key", "key": "..."}}`, which takes precedence.
A key that looks like an environment variable name and is set in the
environment resolves to that variable. `openai-completions` is the supported
API.

If the working directory has an `AGENTS.md` (up to 64 KiB) it is appended to
the system prompt.

## Framing

Lines are delimited by LF only; a trailing CR is stripped. Every command may
carry an `id`, which its response echoes:

```json
{"id": "1", "type": "prompt", "message": "List the Go files"}
{"id": "1", "type": "response", "command": "prompt", "success": true}
```

A failed command answers `success: false` with `error`. A line that is not
JSON answers `{"type": "response", "command": "parse", "success": false, ...}`.

## Commands

| Command | Behaviour |
| --- | --- |
| `prompt` | Start working on `message`. While working, `streamingBehavior` must be `steer` or `followUp` and the message is queued; otherwise the command fails |
| `steer`, `follow_up` | Queue a message for after the current prompt (starts immediately when idle). Steering is currently delivered when the prompt ends, not between model rounds |
| `abort` | Cancel the running prompt, drop queued messages and wait until idle. Text already streamed is kept as an assistant message with `stopReason: "aborted"` |
| `get_state` | Model, thinking level, streaming flag, session file, id and name, queue size |
| `get_messages` | The active conversation |
| `new_session`, `switch_session` | Start an unsaved session, or open `sessionPath` |
| `get_available_models`, `set_model`, `cycle_model` | Model selection; changes are recorded in the session |
| `set_thinking_level`, `cycle_thinking_level`, `get_available_thinking_levels` | Reasoning level for models with `reasoning: true` |
| `set_steering_mode`, `set_follow_up_mode`, `set_auto_compaction`, `set_auto_retry`, `abort_retry` | Accepted and recorded |
| `get_session_stats` | Message counts, token and cost totals, context usage |
| `get_last_assistant_text`, `get_fork_messages`, `set_session_name`, `get_commands` | As named |
| `extension_ui_response` | Accepted; no dialogs are issued yet |

Not supported yet, answered with an error: `compact`, `fork`, `clone`,
`bash`, `abort_bash`, `get_entries`, `get_tree`, `export_html`, and prompts
with `images`.

## Events

One prompt produces, in order:

```
agent_start
turn_start
message_start / message_end            the user message
message_start                          assistant message, empty content
message_update  text_start | text_delta | text_end
message_end                            complete assistant message
tool_execution_start                   one per tool call
tool_execution_end
message_start / message_end            one toolResult message per call
turn_end, turn_start                   when the assistant called tools, the next model round
...
turn_end
agent_end                              all messages added by this prompt
agent_settled                          after the queue is empty
```

`queue_update` reports the pending steering and follow-up messages whenever
they change. Assistant `stopReason` is `stop`, `length`, `toolUse`, `error`
(with `errorMessage`) or `aborted`.

## Session file

Line one is the header
`{"type":"session","version":3,"id":...,"timestamp":...,"cwd":...}`; every
further line is an entry with `id`, `parentId` and `timestamp`, forming a tree
whose last entry is the current leaf. The agent writes `message`,
`model_change`, `thinking_level_change` and `session_info` entries, and reads
`compaction`, `branch_summary` and `custom_message` entries written by other
tools when it rebuilds context. The file is created when the first assistant
message completes.

## Limits of this version

- Text only: images, reasoning text and tool-call argument deltas are not
  streamed.
- No automatic or manual compaction.
- Steering is delivered after the current prompt.
- Tool results are text.
