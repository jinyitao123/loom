package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

// --- prompt parsing ---

func TestParsePromptInstruction(t *testing.T) {
	p, err := parsePrompt([]byte(`{"type":"chat","instruction":"do X","notice":"be brief"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Instruction != "do X" {
		t.Fatalf("instruction = %q", p.Instruction)
	}
	if p.Notice != "be brief" {
		t.Fatalf("notice = %q", p.Notice)
	}
}

func TestParsePromptReadsRecalledMemory(t *testing.T) {
	p, err := parsePrompt([]byte(`{"type":"chat","instruction":"do X","recalled_memory":["user prefers brevity","ticket #42 was urgent"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.RecalledMemory) != 2 || p.RecalledMemory[0] != "user prefers brevity" {
		t.Fatalf("recalled_memory not parsed: %v", p.RecalledMemory)
	}
}

func TestRunAgentFoldsRecalledMemoryIntoSystemPrompt(t *testing.T) {
	var buf bytes.Buffer
	llm := &fakeLLM{responses: []contract.ChatResponse{{Content: "ok"}}}
	deps := runDeps{llm: llm, tools: noTools{}, out: NewEmitter(&buf)}
	p := promptInput{Instruction: "hi", RecalledMemory: []string{"MEMORY_MARKER_42"}}

	if err := runAgent(context.Background(), deps, p, runConfig{model: "deepseek-chat"}); err != nil {
		t.Fatal(err)
	}
	if len(llm.lastMessages) == 0 || llm.lastMessages[0].Role != "system" {
		t.Fatalf("expected a leading system message, got %+v", llm.lastMessages)
	}
	if !strings.Contains(llm.lastMessages[0].Content, "MEMORY_MARKER_42") {
		t.Fatalf("recalled memory not folded into system prompt: %q", llm.lastMessages[0].Content)
	}
}

func TestParsePromptPlainTextFallback(t *testing.T) {
	p, err := parsePrompt([]byte("just a plain instruction"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Instruction != "just a plain instruction" {
		t.Fatalf("instruction = %q", p.Instruction)
	}
}

func TestParsePromptEmptyIsError(t *testing.T) {
	if _, err := parsePrompt([]byte("   \n")); err == nil {
		t.Fatal("want error for empty stdin")
	}
}

func TestParsePromptJSONWithoutInstructionIsError(t *testing.T) {
	if _, err := parsePrompt([]byte(`{"type":"chat"}`)); err == nil {
		t.Fatal("want error for JSON without instruction")
	}
}

// --- provider selection ---

func envFunc(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestBuildLLMDeepseek(t *testing.T) {
	llm, err := buildLLM("deepseek-chat", envFunc(map[string]string{"DEEPSEEK_API_KEY": "k"}))
	if err != nil || llm == nil {
		t.Fatalf("llm=%v err=%v", llm, err)
	}
}

func TestBuildLLMDeepseekMissingKey(t *testing.T) {
	if _, err := buildLLM("deepseek-chat", envFunc(nil)); err == nil {
		t.Fatal("want error when DEEPSEEK_API_KEY missing")
	}
}

func TestBuildLLMOpenAIMissingKey(t *testing.T) {
	if _, err := buildLLM("gpt-4o", envFunc(nil)); err == nil {
		t.Fatal("want error when OPENAI_API_KEY missing")
	}
}

func TestBuildLLMBaseURLOverride(t *testing.T) {
	llm, err := buildLLM("any-model", envFunc(map[string]string{
		"LOOM_BASE_URL": "http://localhost:11434",
		"LOOM_API_KEY":  "x",
	}))
	if err != nil || llm == nil {
		t.Fatalf("llm=%v err=%v", llm, err)
	}
}

// --- run orchestration (with a fake LLM) ---

type fakeLLM struct {
	responses    []contract.ChatResponse
	errs         []error
	calls        int
	lastMessages []contract.Message
}

func (f *fakeLLM) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	f.lastMessages = req.Messages
	i := f.calls
	f.calls++
	if i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	if i < len(f.responses) {
		r := f.responses[i]
		return &r, nil
	}
	return &contract.ChatResponse{}, nil
}

func (f *fakeLLM) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, fmt.Errorf("not implemented")
}

type fakeDispatcher struct {
	tools  []contract.ToolDef
	result contract.ToolResult
}

func (f fakeDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) { return f.tools, nil }
func (f fakeDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	r := f.result
	r.CallID = call.ID
	return &r, nil
}

func TestRunAgentEmitsSessionTextResult(t *testing.T) {
	var buf bytes.Buffer
	llm := &fakeLLM{responses: []contract.ChatResponse{{Content: "the answer"}}}
	p := promptInput{Instruction: "what is X"}
	cfg := runConfig{model: "deepseek-chat", resume: "sess_fixed"}

	err := runAgent(context.Background(), runDeps{llm: llm, tools: noTools{}, out: NewEmitter(&buf)}, p, cfg)
	if err != nil {
		t.Fatal(err)
	}
	lines := parseLines(t, &buf)
	if lines[0]["type"] != "session_init" || lines[0]["session_id"] != "sess_fixed" {
		t.Fatalf("first event: %v", lines[0])
	}
	last := lines[len(lines)-1]
	if last["type"] != "result" || last["status"] != "completed" || last["output"] != "the answer" {
		t.Fatalf("last event: %v", last)
	}
	if !hasEvent(lines, "text", "text", "the answer") {
		t.Fatalf("missing text event: %v", lines)
	}
}

func TestRunAgentToolFlowEmitsToolEvents(t *testing.T) {
	var buf bytes.Buffer
	// First turn: a tool call. Second turn: final content.
	llm := &fakeLLM{responses: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "c1", Name: "list_issues", Args: `{}`}}},
		{Content: "found 3"},
	}}
	tools := fakeDispatcher{
		tools:  []contract.ToolDef{{Name: "list_issues"}},
		result: contract.ToolResult{Content: "3 issues"},
	}
	p := promptInput{Instruction: "list issues"}
	cfg := runConfig{model: "deepseek-chat", resume: "s"}

	if err := runAgent(context.Background(), runDeps{llm: llm, tools: tools, out: NewEmitter(&buf)}, p, cfg); err != nil {
		t.Fatal(err)
	}
	lines := parseLines(t, &buf)
	if !hasType(lines, "tool_use") {
		t.Fatalf("missing tool_use: %v", lines)
	}
	if !hasType(lines, "tool_result") {
		t.Fatalf("missing tool_result: %v", lines)
	}
	last := lines[len(lines)-1]
	if last["type"] != "result" || last["output"] != "found 3" {
		t.Fatalf("last event: %v", last)
	}
}

func TestRunAgentLLMErrorEmitsFailed(t *testing.T) {
	var buf bytes.Buffer
	llm := &fakeLLM{errs: []error{fmt.Errorf("upstream 500")}}
	p := promptInput{Instruction: "x"}
	cfg := runConfig{model: "deepseek-chat", resume: "s"}

	if err := runAgent(context.Background(), runDeps{llm: llm, tools: noTools{}, out: NewEmitter(&buf)}, p, cfg); err == nil {
		t.Fatal("want error returned")
	}
	lines := parseLines(t, &buf)
	if !hasType(lines, "error") {
		t.Fatalf("missing error event: %v", lines)
	}
	last := lines[len(lines)-1]
	if last["type"] != "result" || last["status"] != "failed" {
		t.Fatalf("last event: %v", last)
	}
}

func TestResolveMCPPath(t *testing.T) {
	// explicit flag wins
	if got := resolveMCPPath("/custom/mcp.json", "/work"); got != "/custom/mcp.json" {
		t.Fatalf("explicit flag: %q", got)
	}
	// default = .loom-mcp.json under cwd
	if got := resolveMCPPath("", "/work"); got != "/work/.loom-mcp.json" {
		t.Fatalf("default under cwd: %q", got)
	}
	// no cwd → bare default
	if got := resolveMCPPath("", ""); got != ".loom-mcp.json" {
		t.Fatalf("bare default: %q", got)
	}
}

func TestRunCmdMCPPreflightFailureEmitsFailedBeforeSession(t *testing.T) {
	p := writeMCPConfig(t, `{"mcpServers":{"weave-dispatch":{"url":"http://[::1","required":true}}}`)
	var buf bytes.Buffer

	exit := runCmdIO(
		[]string{"--model", "test", "--mcp-config", p},
		strings.NewReader(`{"instruction":"dispatch work"}`),
		&buf,
		envFunc(map[string]string{"LOOM_BASE_URL": "http://llm.invalid", "LOOM_API_KEY": "test"}),
	)
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	lines := parseLines(t, &buf)
	if hasType(lines, "session_init") {
		t.Fatalf("preflight failure must happen before session_init: %v", lines)
	}
	if len(lines) != 2 {
		t.Fatalf("events = %v, want error + result only", lines)
	}
	message, _ := lines[0]["message"].(string)
	if lines[0]["type"] != "error" || !strings.Contains(message, `mcp preflight: required server "weave-dispatch" failed:`) {
		t.Fatalf("first event = %v, want mcp preflight error", lines[0])
	}
	if lines[1]["type"] != "result" || lines[1]["status"] != "failed" {
		t.Fatalf("last event = %v, want failed result", lines[1])
	}
}

func TestRunAgentGeneratesSessionIdWhenNoResume(t *testing.T) {
	var buf bytes.Buffer
	llm := &fakeLLM{responses: []contract.ChatResponse{{Content: "hi"}}}
	if err := runAgent(context.Background(), runDeps{llm: llm, tools: noTools{}, out: NewEmitter(&buf)}, promptInput{Instruction: "x"}, runConfig{}); err != nil {
		t.Fatal(err)
	}
	lines := parseLines(t, &buf)
	sid, _ := lines[0]["session_id"].(string)
	if sid == "" {
		t.Fatalf("expected a generated session id, got empty: %v", lines[0])
	}
}

func TestRunAgentResumeLoadsPriorAndSavesSession(t *testing.T) {
	store := newFileStore(t.TempDir())
	if err := stdlib.SaveSession(store, "sess_keep", []contract.Message{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
	}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	llm := &fakeLLM{responses: []contract.ChatResponse{{Content: "second answer"}}}
	deps := runDeps{llm: llm, tools: noTools{}, store: store, out: NewEmitter(&buf)}

	if err := runAgent(context.Background(), deps, promptInput{Instruction: "second question"}, runConfig{model: "deepseek-chat", resume: "sess_keep"}); err != nil {
		t.Fatal(err)
	}

	var contents []string
	for _, m := range llm.lastMessages {
		contents = append(contents, m.Content)
	}
	joined := strings.Join(contents, "|")
	if !strings.Contains(joined, "first question") || !strings.Contains(joined, "second question") {
		t.Fatalf("LLM did not receive prior + new messages: %v", contents)
	}

	saved, _ := stdlib.LoadSession(store, "sess_keep")
	if len(saved) < 3 || saved[len(saved)-1].Content != "second answer" {
		t.Fatalf("session not updated with the new assistant reply: %+v", saved)
	}
}

func TestRunAgentRichConfigBuildsSystemPrompt(t *testing.T) {
	var buf bytes.Buffer
	llm := &fakeLLM{responses: []contract.ChatResponse{{Content: "ok"}}}
	spec := &stdlib.AgentSpec{SystemPrompt: "ROLE_MARKER_CLERK"}
	deps := runDeps{llm: llm, tools: noTools{}, spec: spec, out: NewEmitter(&buf)}

	if err := runAgent(context.Background(), deps, promptInput{Instruction: "hi"}, runConfig{model: "deepseek-chat"}); err != nil {
		t.Fatal(err)
	}
	if len(llm.lastMessages) == 0 || llm.lastMessages[0].Role != "system" {
		t.Fatalf("expected a leading system message, got %+v", llm.lastMessages)
	}
	if !strings.Contains(llm.lastMessages[0].Content, "ROLE_MARKER_CLERK") {
		t.Fatalf("system prompt missing identity: %q", llm.lastMessages[0].Content)
	}
}

// helpers

func hasType(lines []map[string]any, typ string) bool {
	for _, l := range lines {
		if l["type"] == typ {
			return true
		}
	}
	return false
}

func hasEvent(lines []map[string]any, typ, key, val string) bool {
	for _, l := range lines {
		if l["type"] == typ && l[key] == val {
			return true
		}
	}
	return false
}
