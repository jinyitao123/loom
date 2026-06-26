package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/deepseek"
	"github.com/jinyitao123/loom/provider/openai"
	"github.com/jinyitao123/loom/stdlib"
)

// sink is the event output surface. *Emitter (stream-json) and textEmitter
// (human-readable) both satisfy it.
type sink interface {
	SessionInit(id string) error
	Text(text string) error
	Thinking(text string) error
	ToolUse(id, name string, input json.RawMessage) error
	ToolResult(id, output, status string) error
	TurnEnd(sessionID string) error
	Error(message string) error
	Result(status, output, sessionID string) error
}

// runConfig holds the parsed `loom run` flags.
type runConfig struct {
	format      string
	cwd         string
	model       string
	resume      string
	bypass      bool
	mcpConfig   string
	agentConfig string
	profile     string
}

// resolveCwdFile picks a config file: an explicit flag wins, else <name> under
// the agent cwd (bare <name> when no cwd).
func resolveCwdFile(flag, cwd, name string) string {
	if flag != "" {
		return flag
	}
	if cwd != "" {
		return filepath.Join(cwd, name)
	}
	return name
}

// resolveMCPPath picks the MCP config file (.loom-mcp.json by default).
func resolveMCPPath(flag, cwd string) string { return resolveCwdFile(flag, cwd, ".loom-mcp.json") }

// promptInput is the JSON the weave daemon writes to stdin. Only the fields we
// consume in v1 are modeled; unknown fields are ignored.
type promptInput struct {
	Type        string `json:"type"`
	Instruction string `json:"instruction"`
	Notice      string `json:"notice"`
	// RecalledMemory is semantic memory the weave daemon recalled for this task
	// (memory recall/remember runs daemon-side, around the spawn, for every
	// provider). loom just folds it into the system prompt.
	RecalledMemory []string `json:"recalled_memory"`
}

// parsePrompt reads the stdin payload. A JSON object must carry "instruction";
// anything else is treated as a plain-text instruction (human-testing
// convenience).
func parsePrompt(raw []byte) (promptInput, error) {
	var p promptInput
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return p, fmt.Errorf("empty prompt on stdin")
	}
	if trimmed[0] == '{' {
		if err := json.Unmarshal(trimmed, &p); err != nil {
			return p, fmt.Errorf("invalid prompt JSON: %w", err)
		}
		if p.Instruction == "" {
			return p, fmt.Errorf("prompt JSON has no 'instruction' field")
		}
		return p, nil
	}
	p.Instruction = string(trimmed)
	return p, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// buildLLM selects a provider from the model id + environment. A LOOM_BASE_URL
// override targets any OpenAI-compatible endpoint (Ollama, vLLM, OpenRouter…).
func buildLLM(model string, getenv func(string) string) (contract.LLM, error) {
	if base := getenv("LOOM_BASE_URL"); base != "" {
		key := firstNonEmpty(getenv("LOOM_API_KEY"), getenv("OPENAI_API_KEY"))
		opts := []openai.Option{openai.WithBaseURL(base), openai.WithDefaultModel(model)}
		if getenv("LOOM_JSON_OBJECT_MODE") != "" {
			opts = append(opts, openai.WithJSONObjectMode())
		}
		return openai.New(key, opts...), nil
	}
	switch {
	case strings.HasPrefix(model, "deepseek"):
		key := firstNonEmpty(getenv("DEEPSEEK_API_KEY"), getenv("LOOM_API_KEY"))
		if key == "" {
			return nil, fmt.Errorf("DEEPSEEK_API_KEY not set (model %q)", model)
		}
		return deepseek.New(key), nil
	default:
		key := firstNonEmpty(getenv("OPENAI_API_KEY"), getenv("LOOM_API_KEY"))
		if key == "" {
			return nil, fmt.Errorf("OPENAI_API_KEY not set (model %q)", model)
		}
		return openai.New(key, openai.WithDefaultModel(model)), nil
	}
}

// noTools is the v1 empty tool dispatcher. MCP wiring (reading .loom-mcp.json
// in cwd → an mcphost HTTP dispatcher) is a follow-up (weave C.7).
type noTools struct{}

func (noTools) ListTools(context.Context) ([]contract.ToolDef, error) { return nil, nil }
func (noTools) Dispatch(context.Context, contract.ToolCall) (*contract.ToolResult, error) {
	return nil, fmt.Errorf("no tools configured (MCP wiring is a follow-up)")
}

// toolEventHook turns loom's per-tool Pre/Post hooks into tool_use / tool_result
// events on the sink. With noTools this is dormant; it activates once MCP tools
// are wired in.
func toolEventHook(out sink) contract.ToolHook {
	return contract.ToolHook{
		Pre: func(_ context.Context, call contract.ToolCall) (contract.ToolCall, error) {
			var input json.RawMessage
			if call.Args != "" {
				input = json.RawMessage(call.Args)
			}
			_ = out.ToolUse(call.ID, call.Name, input)
			return call, nil
		},
		Post: func(_ context.Context, _ contract.ToolCall, result *contract.ToolResult) error {
			status := "success"
			if result.IsError {
				status = "error"
			}
			_ = out.ToolResult(result.CallID, result.Content, status)
			return nil
		},
	}
}

// runDeps bundles the runtime collaborators for an agent turn so the signature
// stays small as features (tools, session store, rich config) are added.
type runDeps struct {
	llm   contract.LLM
	tools contract.ToolDispatcher
	store loom.Store        // nil = no session persistence
	spec  *stdlib.AgentSpec // nil = no rich config (identity/skills/profiles)
	out   sink
}

// runAgent drives one agent turn:
//
//	session_init → (load prior session) → assemble system prompt →
//	streaming tool-loop (text deltas + tool events) → (save session) → result
//
// loom.Graph.Run is synchronous; text is streamed by wrapping the LLM
// (streamingLLM). Tool calls surface via ToolHooks. Session continuity (--resume)
// uses the injected store.
func runAgent(ctx context.Context, d runDeps, p promptInput, cfg runConfig) error {
	sessionID := cfg.resume
	if sessionID == "" {
		sessionID = "lm_" + uuid.NewString()
	}
	_ = d.out.SessionInit(sessionID)

	// Conversation continuity: load prior turns when resuming.
	var priorMsgs []contract.Message
	if d.store != nil && cfg.resume != "" {
		priorMsgs, _ = stdlib.LoadSession(d.store, sessionID)
	}
	messages := append(priorMsgs, contract.Message{Role: "user", Content: p.Instruction})
	state := loom.State{"messages": messages, "last_user_message": p.Instruction}

	// Rich config: assemble the system prompt from identity / skills / profile.
	if d.spec != nil {
		assemble := stdlib.NewPromptAssembleStep(stdlib.PromptConfig{
			Identity:     specIdentity(d.spec),
			Skills:       d.spec.Skills,
			Profile:      cfg.profile,
			ProfilesMap:  d.spec.Profiles,
			SkillMatcher: &stdlib.KeywordMatcher{},
		})
		if delta, aerr := assemble(ctx, state); aerr == nil {
			for k, v := range delta {
				state[k] = v
			}
		}
	}
	// Fold recalled memory + the prompt's notice into the system prompt.
	var extras []string
	if len(p.RecalledMemory) > 0 {
		extras = append(extras, "## Relevant memory from past tasks\n"+strings.Join(p.RecalledMemory, "\n"))
	}
	if p.Notice != "" {
		extras = append(extras, p.Notice)
	}
	if len(extras) > 0 {
		parts := extras
		if sp, _ := state["__system_prompt"].(string); sp != "" {
			parts = append([]string{sp}, extras...)
		}
		state["__system_prompt"] = strings.Join(parts, "\n\n")
	}

	streaming := &streamingLLM{inner: d.llm, out: d.out}
	step := stdlib.NewToolLoopStep(streaming, d.tools, stdlib.ToolLoopOpts{
		Model:     cfg.model,
		ToolHooks: []contract.ToolHook{toolEventHook(d.out)},
	})
	result, err := step(ctx, state)
	if err != nil {
		_ = d.out.Error(err.Error())
		_ = d.out.Result("failed", "", sessionID)
		return err
	}

	output := stdlib.GetString(result, "output", "")
	if d.store != nil {
		finalMsgs := append(messages, contract.Message{Role: "assistant", Content: output})
		_ = stdlib.SaveSession(d.store, sessionID, finalMsgs)
	}
	_ = d.out.Result("completed", output, sessionID)
	return nil
}

// textEmitter is the human-readable sink for `--format text`.
type textEmitter struct{ w io.Writer }

func (t textEmitter) SessionInit(string) error { return nil }
func (t textEmitter) Text(s string) error      { _, err := fmt.Fprint(t.w, s); return err }
func (t textEmitter) Thinking(string) error    { return nil }
func (t textEmitter) ToolUse(_, name string, input json.RawMessage) error {
	_, err := fmt.Fprintf(t.w, "\n→ %s %s\n", name, string(input))
	return err
}
func (t textEmitter) ToolResult(_, output, _ string) error {
	_, err := fmt.Fprintf(t.w, "← %s\n", output)
	return err
}
func (t textEmitter) TurnEnd(string) error { return nil }
func (t textEmitter) Error(m string) error {
	_, err := fmt.Fprintf(t.w, "\nERROR: %s\n", m)
	return err
}
func (t textEmitter) Result(status, _, _ string) error {
	_, err := fmt.Fprintf(t.w, "\n[%s]\n", status)
	return err
}

func newSink(format string, w io.Writer) sink {
	if format == "text" {
		return textEmitter{w: w}
	}
	return NewEmitter(w)
}

// runCmd implements `loom run`, wired to the process's real stdio/env.
func runCmd(args []string) int {
	return runCmdIO(args, os.Stdin, os.Stdout, os.Getenv)
}

// runCmdIO is the testable core of `loom run` — stdio and env are injected so it
// can be driven in-process (the subprocess E2E tests cover the real binary; this
// covers the orchestration logic).
func runCmdIO(args []string, stdin io.Reader, stdout io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	format := fs.String("format", "stream-json", "output format: stream-json | text")
	cwd := fs.String("cwd", "", "working directory for the agent")
	model := fs.String("model", "", "model id (e.g. deepseek-chat, gpt-4o)")
	resume := fs.String("resume", "", "resume an existing session id")
	bypass := fs.Bool("bypass-permissions", false, "auto-allow all tool calls (non-interactive)")
	mcpConfig := fs.String("mcp-config", "", "path to .loom-mcp.json (default: <cwd>/.loom-mcp.json)")
	agentConfig := fs.String("agent-config", "", "path to the agent spec JSON (default: <cwd>/.loom-agent.json)")
	profile := fs.String("profile", "", "active profile name")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := runConfig{format: *format, cwd: *cwd, model: *model, resume: *resume, bypass: *bypass, mcpConfig: *mcpConfig, agentConfig: *agentConfig, profile: *profile}

	out := newSink(cfg.format, stdout)

	fail := func(msg string) int {
		_ = out.Error(msg)
		_ = out.Result("failed", "", cfg.resume)
		return 1
	}

	raw, err := io.ReadAll(stdin)
	if err != nil {
		return fail(fmt.Sprintf("read stdin: %v", err))
	}
	p, err := parsePrompt(raw)
	if err != nil {
		return fail(err.Error())
	}
	llm, err := buildLLM(cfg.model, getenv)
	if err != nil {
		return fail(err.Error())
	}
	tools, err := loadMCPDispatcher(resolveMCPPath(cfg.mcpConfig, cfg.cwd))
	if err != nil {
		return fail(err.Error())
	}
	spec, err := loadAgentSpec(resolveCwdFile(cfg.agentConfig, cfg.cwd, ".loom-agent.json"))
	if err != nil {
		return fail(err.Error())
	}
	sessionDir := resolveCwdFile("", cfg.cwd, ".loom-sessions")
	if cfg.cwd != "" {
		if err := os.Chdir(cfg.cwd); err != nil {
			return fail(fmt.Sprintf("chdir %s: %v", cfg.cwd, err))
		}
	}
	deps := runDeps{llm: llm, tools: tools, store: newFileStore(sessionDir), spec: spec, out: out}
	if err := runAgent(context.Background(), deps, p, cfg); err != nil {
		return 1
	}
	return 0
}
