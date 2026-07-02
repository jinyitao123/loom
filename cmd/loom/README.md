# loom CLI

The `loom` binary is the [weave](https://github.com/jinyitao123/Weave) daemon's
spawn-harness backend — the **digital-avatar engine**. It reads a prompt JSON on
stdin, runs one agent turn on the Loom engine, and emits a stdout-json (NDJSON)
event stream the daemon parses (same integration shape as `claude` / `opencode`).

## Install

```sh
# from a GitHub Release (linux / macOS):
curl -fsSL https://raw.githubusercontent.com/jinyitao123/loom/main/install.sh | sh

# or with Go (any platform):
go install github.com/jinyitao123/loom/cmd/loom@latest
```

Verify: `loom version`.

The weave daemon finds it on `PATH` (or via `WEAVE_LOOM_PATH`) and auto-registers
a `loom` runtime.

## Usage

```sh
loom run --format stream-json --cwd <agent-workdir> --bypass-permissions \
         [--model deepseek-chat] [--resume <sessionId>] < prompt.json
loom version
```

Config it reads from the agent workdir (written by the daemon):

- `.loom-agent.json` — the avatar's identity / profile / skills (system prompt).
- `.loom-mcp.json`   — MCP tool servers (email / calendar / issue …).
- `.loom-sessions/`  — per-session conversation history (for `--resume`).

Provider creds come from the environment: `DEEPSEEK_API_KEY`, `OPENAI_API_KEY`,
or `LOOM_BASE_URL` + `LOOM_API_KEY` for any OpenAI-compatible endpoint.

## Output events (stdout-json)

`session_init` · `text` · `thinking` · `tool_use` · `tool_result` · `turn_end` ·
`error` · `result`. This wire format is pinned by a cross-repo contract test
(`cmd/loom/contract_test.go` ↔ weave `loom.contract.test.ts`).

## Develop

```sh
go test ./cmd/loom/            # 91 tests: unit + E2E + contract + fuzz + security
go test -race ./cmd/loom/      # race + goleak
```

CI (`.github/workflows/loom-cli.yml`) runs the suite on linux/macOS/windows plus
staticcheck / gosec / golangci-lint. Releases are cut by tagging `vX.Y.Z`
(`.github/workflows/release.yml` → goreleaser).
