package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/interactive"
	"github.com/jinyitao123/loom/localtools"
	"github.com/jinyitao123/loom/mcpclient"
	"github.com/jinyitao123/loom/provider/anthropic"
	"github.com/jinyitao123/loom/provider/openai"
)

// maxContextFile bounds an AGENTS.md file folded into the system prompt.
const maxContextFile = 64 << 10

// agentCmd runs `loom agent --mode rpc`: a long-lived interactive agent that
// reads JSONL commands on stdin and writes responses and events on stdout.
// See docs/interactive.md for the protocol.
// agentCmd 运行 `loom agent --mode rpc`：长期运行的交互式智能体，stdin 读 JSONL 命令，
// stdout 写响应与事件，协议见 docs/interactive.md。
func agentCmd(args []string) int {
	return agentCmdIO(args, os.Stdin, os.Stdout, os.Stderr, os.Getenv)
}

func agentCmdIO(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", "rpc", "protocol mode (only rpc)")
	cwd := fs.String("cwd", "", "working directory (default: current directory)")
	session := fs.String("session", "", "session file to continue")
	sessionDir := fs.String("session-dir", "", "directory for new session files")
	provider := fs.String("provider", "", "model provider from models.json")
	model := fs.String("model", "", "model id from models.json")
	thinking := fs.String("thinking", "", "thinking level: off, minimal, low, medium, high, xhigh, max")
	tools := fs.String("tools", strings.Join(localtools.Default, ","), "comma-separated local tools ("+strings.Join(localtools.All, ",")+")")
	var skillPaths multiFlag
	fs.Var(&skillPaths, "skill", "skill file or directory (repeatable)")
	var mcpConfigs multiFlag
	fs.Var(&mcpConfigs, "mcp-config", "MCP server configuration file (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *mode != "rpc" {
		fmt.Fprintf(stderr, "loom agent: unsupported mode %q\n", *mode)
		return 2
	}
	workdir := *cwd
	if workdir == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "loom agent: %v\n", err)
			return 1
		}
		workdir = wd
	}
	workdir, _ = filepath.Abs(workdir)

	agentDir := getenv("LOOM_AGENT_DIR")
	if agentDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(stderr, "loom agent: %v\n", err)
			return 1
		}
		agentDir = filepath.Join(home, ".loom", "agent")
	}
	dir := *sessionDir
	if dir == "" {
		root := getenv("LOOM_SESSION_DIR")
		if root == "" {
			root = filepath.Join(agentDir, "sessions")
		}
		dir = interactive.SessionDir(root, workdir)
	}

	catalog, err := interactive.LoadCatalog(agentDir, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "loom agent: %v\n", err)
		return 1
	}
	var names []string
	for _, name := range strings.Split(*tools, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	dispatcher, err := localtools.New(workdir, names...)
	if err != nil {
		fmt.Fprintf(stderr, "loom agent: %v\n", err)
		return 2
	}

	skills := interactive.LoadSkills("path", skillPaths...)
	skills = appendSkills(skills, interactive.LoadSkills("project", filepath.Join(workdir, ".loom", "skills")))
	skills = appendSkills(skills, interactive.LoadSkills("user", filepath.Join(agentDir, "skills")))

	server := interactive.NewServer(stdout)
	servers, err := mcpclient.LoadConfig(append([]string{filepath.Join(agentDir, "mcp.json"), filepath.Join(workdir, ".loom", "mcp.json")}, mcpConfigs...)...)
	if err != nil {
		fmt.Fprintf(stderr, "loom agent: %v\n", err)
		return 1
	}
	mcp, failures := mcpclient.Connect(context.Background(), servers, mcpclient.Options{
		ClientVersion: version,
		Cwd:           workdir,
		Elicit: func(ctx context.Context, source, message string, schema json.RawMessage) (mcpclient.ElicitResult, error) {
			action, content, err := server.Elicit(ctx, source, message, schema)
			return mcpclient.ElicitResult{Action: action, Content: content}, err
		},
	})
	defer mcp.Close()
	for _, failure := range failures {
		fmt.Fprintf(stderr, "loom agent: %v\n", failure)
	}

	agent, err := interactive.New(interactive.Options{
		Cwd:          workdir,
		SessionDir:   dir,
		Catalog:      catalog,
		NewLLM:       newModelClient,
		Tools:        interactive.NewMultiDispatcher(dispatcher, mcp),
		SystemPrompt: systemPrompt(workdir, names) + interactive.SkillsPrompt(skills) + mcp.Instructions(),
		Skills:       skills,
		Emit:         server.Emit,
		OnTurnStart:  mcp.TurnStarted,
	}, *session, *provider, *model, *thinking)
	if err != nil {
		fmt.Fprintf(stderr, "loom agent: %v\n", err)
		return 1
	}
	dispatcher.OnProgress(agent.ToolProgress)
	server.Attach(agent)
	for _, failure := range failures {
		server.Emit(map[string]any{"type": "extension_error", "extensionPath": "mcp", "event": "connect", "error": failure.Error()})
	}
	if err := server.Serve(stdin); err != nil {
		fmt.Fprintf(stderr, "loom agent: %v\n", err)
		return 1
	}
	return 0
}

// newModelClient builds the client for a configured model: OpenAI-compatible
// chat completions or the Anthropic Messages API.
func newModelClient(model interactive.Model, apiKey string) (contract.LLM, error) {
	switch model.API {
	case "openai-completions":
		opts := []openai.Option{openai.WithDefaultModel(model.ID)}
		if model.BaseURL != "" {
			opts = append(opts, openai.WithBaseURL(model.BaseURL), openai.WithChatPath("/chat/completions"))
		}
		return openai.New(apiKey, opts...), nil
	case "anthropic-messages":
		return anthropic.New(apiKey, anthropic.WithBaseURL(model.BaseURL), anthropic.WithDefaultModel(model.ID)), nil
	default:
		return nil, fmt.Errorf("model API %q is not supported by the Loom kernel yet", model.API)
	}
}

// systemPrompt describes the environment and tools, followed by the
// working directory's AGENTS.md when present.
func systemPrompt(cwd string, tools []string) string {
	var b strings.Builder
	b.WriteString("You are an expert assistant working on the user's computer. You help by reading files, running commands, editing code and writing new files.\n\n")
	b.WriteString("Available tools: " + strings.Join(tools, ", ") + ".\n\n")
	b.WriteString("Guidelines:\n")
	b.WriteString("- Read a file before you change it, and prefer edit for targeted changes over rewriting whole files.\n")
	b.WriteString("- Keep answers concise; show file paths when you mention files.\n")
	b.WriteString("- Do not guess file contents or command results; use the tools.\n\n")
	fmt.Fprintf(&b, "Current date: %s\nPlatform: %s/%s\nCurrent working directory: %s\n", time.Now().Format("2006-01-02"), runtime.GOOS, runtime.GOARCH, cwd)
	if data, err := os.ReadFile(filepath.Join(cwd, "AGENTS.md")); err == nil && len(data) <= maxContextFile {
		b.WriteString("\n# Project instructions (AGENTS.md)\n\n")
		b.Write(data)
		b.WriteString("\n")
	}
	return b.String()
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// appendSkills adds skills whose names are not taken yet; earlier sources win.
func appendSkills(skills, more []interactive.Skill) []interactive.Skill {
	taken := map[string]bool{}
	for _, s := range skills {
		taken[s.Name] = true
	}
	for _, s := range more {
		if !taken[s.Name] {
			taken[s.Name] = true
			skills = append(skills, s)
		}
	}
	return skills
}
