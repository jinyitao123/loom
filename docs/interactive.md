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
           [--provider NAME] [--model ID] [--thinking LEVEL] [--tools read,bash,edit,write] \n           [--skill PATH]... [--mcp-config FILE]...
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

A model whose `input` lists `"image"` receives images (from prompts and from
`read`) as image parts; other models receive a short note in their place.

## Skills

A skill is a `SKILL.md` file whose front matter names it and describes when to
use it (`name`, `description`). Skills come from `--skill` paths, then
`<cwd>/.loom/skills`, then `<configuration>/skills`; a path may be a
`SKILL.md`, a directory holding one, or a directory of such directories, and
the first skill with a name wins. The system prompt lists every skill with its
location so the model reads the file when a task matches; a prompt
`/skill:<name> [args]` inlines the skill followed by the arguments, and
`get_commands` lists them as `skill:<name>`.

## Tool servers (MCP)

Model Context Protocol servers add tools. They are configured in
`<configuration>/mcp.json`, `<cwd>/.loom/mcp.json` and `--mcp-config` files,
later files replacing a server of the same name:

```json
{"mcpServers": {
  "files": {"command": "npx", "args": ["-y", "some-mcp-server"], "env": {"TOKEN": "${FILES_TOKEN}"}},
  "host":  {"url": "http://127.0.0.1:8123/mcp", "headers": {"Authorization": "Bearer ${HOST_TOKEN}"}, "toolPrefix": ""}
}}
```

- `command` starts a stdio server; `url` speaks streamable HTTP (JSON or event
  stream answers, `Mcp-Session-Id` sessions). `${VAR}` expands from the
  environment. `disabled: true` skips a server.
- Tools are named `mcp__<server>__<tool>`; `toolPrefix` replaces the prefix
  (`""` keeps the server's names). `readOnlyHint` lets read-only tools run in
  parallel.
- A server's `instructions` are appended to the system prompt.
- When a server asks for user input (elicitation), Loom shows it through the
  dialog sub-protocol below: enumerations become selects (two to five of them
  one questionnaire), booleans confirmations, other fields text input.
- A server that fails to start is reported on stderr and as an
  `extension_error` event; the agent runs without it. A
  `tools/list_changed` notification refreshes its tools before the next
  model round.

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
| `prompt` | Start working on `message` and optional `images` (PNG, JPEG, GIF or WebP, base64, up to 8 of 20 MB each). While working, `streamingBehavior` must be `steer` or `followUp` and the message is queued; otherwise the command fails |
| `steer`, `follow_up` | Queue a message (starts immediately when idle). Steering is delivered after the current round's tool calls, before the next model request; follow-ups run after the prompt finishes. `set_steering_mode` / `set_follow_up_mode` choose one at a time or all |
| `abort` | Cancel the running prompt, drop queued messages and wait until idle. Text already streamed is kept as an assistant message with `stopReason: "aborted"` |
| `get_state` | Model, thinking level, streaming flag, session file, id and name, queue size |
| `get_messages` | The active conversation |
| `new_session`, `switch_session` | Start an unsaved session, or open `sessionPath` |
| `get_available_models`, `set_model`, `cycle_model` | Model selection; changes are recorded in the session |
| `set_thinking_level`, `cycle_thinking_level`, `get_available_thinking_levels` | Reasoning level for models with `reasoning: true` |
| `compact` | Summarize older history now, with optional `customInstructions`; fails while a prompt runs |
| `set_steering_mode`, `set_follow_up_mode`, `set_auto_compaction` | Queue delivery and automatic compaction |
| `set_auto_retry`, `abort_retry` | Accepted and recorded |
| `get_session_stats` | Message counts, token and cost totals, context usage |
| `get_last_assistant_text`, `get_fork_messages`, `set_session_name`, `get_commands` | As named |
| `extension_ui_response` | Answer to a dialog (`value`, `confirmed` or `cancelled`); no response is sent |

Not supported yet, answered with an error: `fork`, `clone`, `bash`,
`abort_bash`, `get_entries`, `get_tree`, `export_html`.

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

A steering message appears as `message_start/end` of a user message
between a round's tool results and the next assistant message.

`queue_update` reports the pending steering and follow-up messages whenever
they change. `compaction_start` (`reason`: `threshold`, `overflow` or
`manual`) and `compaction_end` (`result`, `aborted`, `willRetry`,
`errorMessage`) bracket a compaction. Assistant `stopReason` is `stop`, `length`, `toolUse`, `error`
(with `errorMessage`) or `aborted`.

## Dialogs

`extension_ui_request` events carry an `id` and a `method`: `select`
(`title`, `options`), `confirm` (`title`, `message`) or `input` (`title`,
optional `placeholder`). The host answers with
`{"type": "extension_ui_response", "id": ..., "value": ...}`, `"confirmed": true|false`
or `"cancelled": true`. Selects belonging to one questionnaire carry
`__prime_ask_user__<group>:<index>:<total>` as their first option.

## Compaction

When the conversation's context passes the model's window minus 16384 tokens
(checked before every model round), when a request fails because the context
is too long (followed by one retry), or on `compact`, the agent keeps the
newest 20000 tokens verbatim and asks the model to summarize the rest in a
structured checkpoint (goal, constraints, progress, decisions, next steps,
critical context), listing the files read and modified. The summary is stored
as a `compaction` entry whose `firstKeptEntryId` marks where verbatim history
resumes; later compactions update the previous summary. Kept history never
starts at a tool result.

## Session file

Line one is the header
`{"type":"session","version":3,"id":...,"timestamp":...,"cwd":...}`; every
further line is an entry with `id`, `parentId` and `timestamp`, forming a tree
whose last entry is the current leaf. The agent writes `message`,
`model_change`, `thinking_level_change`, `session_info` and `compaction`
entries, and also reads `branch_summary` and `custom_message` entries written
by other tools when it rebuilds context. The file is created when the first assistant
message completes.

## Limits of this version

- Reasoning text and tool-call argument deltas are not streamed; only
  answer text is.
- Images are not resized; large images count fully against the context.
- `.gitignore` handling in `grep` and `find` covers the common patterns of
  the root file only.
