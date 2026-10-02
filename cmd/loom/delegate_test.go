package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/loom/stdlib/compiler"
)

// scriptLLM returns canned responses in order (then a terminal content reply).
type scriptLLM struct {
	responses []contract.ChatResponse
	i         int
}

func (m *scriptLLM) Chat(_ context.Context, _ contract.ChatRequest) (*contract.ChatResponse, error) {
	if m.i >= len(m.responses) {
		return &contract.ChatResponse{Content: "done"}, nil
	}
	r := m.responses[m.i]
	m.i++
	return &r, nil
}
func (m *scriptLLM) Stream(_ context.Context, _ contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, nil
}

func twoSubSpec() *stdlib.AgentSpec {
	return &stdlib.AgentSpec{
		Identity: stdlib.IdentitySpec{Core: "you coordinate"},
		SubAgents: []stdlib.SubAgentRef{
			{Name: "researcher", Description: "researches facts"},
			{Name: "writer", Description: "writes prose"},
		},
	}
}

func leafResolver(ran *string) func(string) (*loom.Graph, error) {
	return func(name string) (*loom.Graph, error) {
		child := loom.NewGraph("child:"+name, "leaf")
		child.AddStep("leaf", func(_ context.Context, _ loom.State) (loom.State, error) {
			if ran != nil {
				*ran = name
			}
			return loom.State{"output": "CHILD_" + name}, nil
		}, loom.End())
		return child, nil
	}
}

func entryState() loom.State {
	return loom.State{
		"messages":          []contract.Message{{Role: "user", Content: "hi"}},
		"last_user_message": "hi",
	}
}

// THE CORE PROOF: a model `delegate` call sets __delegate_to so the compiler's
// previously-dead deterministic router fires and the sub-agent's output bubbles up.
func TestOrchestration_DelegateFiresRouter(t *testing.T) {
	sig := &delegateSignal{}
	llm := &scriptLLM{responses: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "1", Name: delegateToolName, Args: `{"agent":"researcher","task":"find X"}`}}},
		{Content: "delegated"}, // post-tool reply ends the loop
	}}
	spec := twoSubSpec()
	dd := newDelegateDispatcher(nil, spec.SubAgents, sig)
	var childRan string
	g, err := compiler.CompileAgent(spec, llm, dd, compiler.CompileOpts{
		ChatStepFactory:  delegateChatStepFactory(sig),
		SubAgentResolver: leafResolver(&childRan),
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res, err := g.Run(context.Background(), entryState(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if childRan != "researcher" {
		t.Fatalf("expected sub_researcher to run, got %q", childRan)
	}
	if got := stdlib.GetString(res.State, "output", ""); got != "CHILD_researcher" {
		t.Fatalf("output = %q, want CHILD_researcher (sub-agent answer)", got)
	}
}

// No delegate call → the parent answers directly; no sub-agent runs.
func TestOrchestration_NoDelegateAnswersDirectly(t *testing.T) {
	sig := &delegateSignal{}
	llm := &scriptLLM{responses: []contract.ChatResponse{{Content: "DIRECT_ANSWER"}}}
	spec := twoSubSpec()
	dd := newDelegateDispatcher(nil, spec.SubAgents, sig)
	var childRan string
	g, err := compiler.CompileAgent(spec, llm, dd, compiler.CompileOpts{
		ChatStepFactory:  delegateChatStepFactory(sig),
		SubAgentResolver: leafResolver(&childRan),
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res, err := g.Run(context.Background(), entryState(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if childRan != "" {
		t.Fatalf("no sub-agent should run, but %q did", childRan)
	}
	if got := stdlib.GetString(res.State, "output", ""); got != "DIRECT_ANSWER" {
		t.Fatalf("output = %q, want DIRECT_ANSWER", got)
	}
}

// Exercise the actual CLI assembly, including child specs on disk and final
// events; the lower-level router fixtures above use synthetic child graphs.
func TestRunAgentOrchestrationLoadsChildAndReportsOutcome(t *testing.T) {
	for _, test := range []struct {
		name       string
		child      string
		responses  []contract.ChatResponse
		modelError error
		wantOutput string
		wantError  string
		delegated  bool
	}{
		{
			name: "direct answer retains parent context", child: `{"system_prompt":"CHILD_IDENTITY"}`,
			responses: []contract.ChatResponse{{Content: "parent answer"}}, wantOutput: "parent answer",
		},
		{
			name: "delegated answer comes from child", child: `{"system_prompt":"CHILD_IDENTITY"}`,
			responses: []contract.ChatResponse{
				{ToolCalls: []contract.ToolCall{{ID: "delegate-1", Name: delegateToolName, Args: `{"agent":"worker","task":"inspect the sample"}`}}},
				{Content: "parent handoff"}, {Content: "child answer"},
			}, wantOutput: "child answer", delegated: true,
		},
		{name: "missing child fails before model", wantError: "spec not found"},
		{name: "invalid child fails before model", child: `{bad json`, wantError: "parse agent-config"},
		{
			name: "model failure reports failed outcome", child: `{"system_prompt":"CHILD_IDENTITY"}`,
			modelError: fmt.Errorf("synthetic model failure"), wantError: "synthetic model failure",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cwd := t.TempDir()
			if test.child != "" {
				childDir := filepath.Join(cwd, ".loom-agents")
				if err := os.Mkdir(childDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(childDir, "worker.json"), []byte(test.child), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			model := &fakeLLM{responses: test.responses}
			if test.modelError != nil {
				model.errs = []error{test.modelError}
			}
			var out bytes.Buffer
			spec := &stdlib.AgentSpec{SystemPrompt: "PARENT_IDENTITY", SubAgents: []stdlib.SubAgentRef{{Name: "worker"}}}
			prompt := promptInput{Instruction: "inspect", Notice: "NOTICE_MARKER", RecalledMemory: []string{"MEMORY_MARKER"}}
			err := runAgent(context.Background(), runDeps{llm: model, tools: noTools{}, spec: spec, out: NewEmitter(&out)}, prompt, runConfig{cwd: cwd, model: "test"})
			lines := parseLines(t, &out)
			if len(lines) < 2 || lines[0]["type"] != "session_init" {
				t.Fatalf("missing session event: %v", lines)
			}
			last := lines[len(lines)-1]
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) || last["type"] != "result" || last["status"] != "failed" || !hasType(lines, "error") {
					t.Fatalf("failure was not reported: err=%v events=%v", err, lines)
				}
				if test.modelError == nil && model.calls != 0 {
					t.Fatalf("invalid child reached model: calls=%d", model.calls)
				}
				return
			}
			if err != nil || last["type"] != "result" || last["status"] != "completed" || last["output"] != test.wantOutput {
				t.Fatalf("incorrect completion: err=%v events=%v", err, lines)
			}
			if model.calls != len(test.responses) || len(model.lastMessages) == 0 || model.lastMessages[0].Role != "system" {
				t.Fatalf("unexpected model execution: calls=%d messages=%v", model.calls, model.lastMessages)
			}
			if test.delegated {
				if !strings.Contains(model.lastMessages[0].Content, "CHILD_IDENTITY") || !hasEvent(lines, "tool_use", "name", delegateToolName) || !hasType(lines, "tool_result") {
					t.Fatalf("child identity or delegation evidence missing: messages=%v events=%v", model.lastMessages, lines)
				}
			} else {
				for _, marker := range []string{"PARENT_IDENTITY", "NOTICE_MARKER", "MEMORY_MARKER"} {
					if !strings.Contains(model.lastMessages[0].Content, marker) {
						t.Fatalf("parent context lost %s", marker)
					}
				}
			}
		})
	}
}

func TestDelegateDispatcher_ListToolsAddsDelegate(t *testing.T) {
	sig := &delegateSignal{}
	inner := stubTools{defs: []contract.ToolDef{{Name: "search"}}}
	dd := newDelegateDispatcher(inner, twoSubSpec().SubAgents, sig)
	tools, err := dd.ListTools(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	names := map[string]bool{}
	var delegateDef *contract.ToolDef
	for i := range tools {
		names[tools[i].Name] = true
		if tools[i].Name == delegateToolName {
			delegateDef = &tools[i]
		}
	}
	if !names["search"] || !names[delegateToolName] {
		t.Fatalf("expected inner + delegate tools, got %v", names)
	}
	// enum lists the sub-agent route keys
	var schema struct {
		Properties struct {
			Agent struct {
				Enum []string `json:"enum"`
			} `json:"agent"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(delegateDef.InputSchema, &schema)
	if len(schema.Properties.Agent.Enum) != 2 {
		t.Fatalf("delegate enum should list 2 sub-agents, got %v", schema.Properties.Agent.Enum)
	}
}

func TestDelegateDispatcher_DispatchSetsSignalAndPassesThrough(t *testing.T) {
	sig := &delegateSignal{}
	passed := false
	inner := stubTools{onDispatch: func(c contract.ToolCall) { passed = true }}
	dd := newDelegateDispatcher(inner, twoSubSpec().SubAgents, sig)

	// delegate → records signal, does not hit inner
	_, _ = dd.Dispatch(context.Background(), contract.ToolCall{ID: "1", Name: delegateToolName, Args: `{"agent":"writer","task":"draft"}`})
	if set, agent, task := sig.read(); !set || agent != "writer" || task != "draft" {
		t.Fatalf("signal not recorded: set=%v agent=%q task=%q", set, agent, task)
	}
	if passed {
		t.Fatalf("delegate call must not reach inner dispatcher")
	}

	// unknown sub-agent → error result, no signal change
	sig2 := &delegateSignal{}
	dd2 := newDelegateDispatcher(nil, twoSubSpec().SubAgents, sig2)
	res, _ := dd2.Dispatch(context.Background(), contract.ToolCall{ID: "2", Name: delegateToolName, Args: `{"agent":"ghost","task":"x"}`})
	if res == nil || !res.IsError {
		t.Fatalf("unknown sub-agent should be an error result")
	}
	if set, _, _ := sig2.read(); set {
		t.Fatalf("unknown sub-agent must not set the signal")
	}

	// normal tool → passes through to inner
	_, _ = dd.Dispatch(context.Background(), contract.ToolCall{ID: "3", Name: "search", Args: `{}`})
	if !passed {
		t.Fatalf("non-delegate call must reach inner dispatcher")
	}
}

func TestDelegateChatStep_RoutesOnlyWhenSignalled(t *testing.T) {
	// inner step that produces a normal answer
	innerStep := func(_ context.Context, _ loom.State) (loom.State, error) {
		return loom.State{"output": "ANSWER"}, nil
	}
	_ = innerStep // documents intent; the factory builds its own inner via NewToolLoopStep

	// signalled → returns __delegate_to, drops output
	sig := &delegateSignal{}
	sig.record("researcher", "task-x")
	step := delegateChatStepFactory(sig)(&scriptLLM{responses: []contract.ChatResponse{{Content: "pre"}}}, stubTools{}, stdlib.ToolLoopOpts{})
	out, err := step(context.Background(), entryState())
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if out["__delegate_to"] != "researcher" {
		t.Fatalf("expected __delegate_to=researcher, got %v", out["__delegate_to"])
	}
	if _, hasOutput := out["output"]; hasOutput {
		t.Fatalf("delegation must drop pre-delegation output")
	}
	if out["last_user_message"] != "task-x" {
		t.Fatalf("expected delegated task as last_user_message, got %v", out["last_user_message"])
	}

	// not signalled → returns the inner answer, no __delegate_to
	sig2 := &delegateSignal{}
	step2 := delegateChatStepFactory(sig2)(&scriptLLM{responses: []contract.ChatResponse{{Content: "ANSWER"}}}, stubTools{}, stdlib.ToolLoopOpts{})
	out2, err := step2(context.Background(), entryState())
	if err != nil {
		t.Fatalf("step2: %v", err)
	}
	if _, has := out2["__delegate_to"]; has {
		t.Fatalf("no signal → must not set __delegate_to")
	}
	if stdlib.GetString(out2, "output", "") != "ANSWER" {
		t.Fatalf("no signal → must return inner output, got %v", out2["output"])
	}
}

// stubTools is a minimal ToolDispatcher for tests.
type stubTools struct {
	defs       []contract.ToolDef
	onDispatch func(contract.ToolCall)
}

func (s stubTools) ListTools(context.Context) ([]contract.ToolDef, error) { return s.defs, nil }
func (s stubTools) Dispatch(_ context.Context, c contract.ToolCall) (*contract.ToolResult, error) {
	if s.onDispatch != nil {
		s.onDispatch(c)
	}
	return &contract.ToolResult{CallID: c.ID, Content: "ok"}, nil
}
