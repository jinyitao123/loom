package compiler

import (
	"context"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

// nilLLM / nilTools are enough to compile a graph (compilation does not call
// them; only Graph.Run would).
type nilLLM struct{}

func (nilLLM) Chat(context.Context, contract.ChatRequest) (*contract.ChatResponse, error) {
	return &contract.ChatResponse{}, nil
}
func (nilLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, nil
}

type nilTools struct{}

func (nilTools) ListTools(context.Context) ([]contract.ToolDef, error) { return nil, nil }
func (nilTools) Dispatch(context.Context, contract.ToolCall) (*contract.ToolResult, error) {
	return &contract.ToolResult{}, nil
}

func stepNames(topo []loom.StepInfo) map[string]bool {
	m := map[string]bool{}
	for _, s := range topo {
		m[s.Name] = true
	}
	return m
}

func TestCompileAgent_StandardTopology(t *testing.T) {
	spec := &stdlib.AgentSpec{Identity: stdlib.IdentitySpec{Core: "you are a helper"}}
	g, err := CompileAgent(spec, nilLLM{}, nilTools{}, CompileOpts{Model: "deepseek-chat"})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	names := stepNames(g.Topology())
	if !names["prompt_assemble"] || !names["chat"] {
		t.Fatalf("standard topology missing core steps: %v", names)
	}
	if names["router"] {
		t.Fatalf("standard agent must not have a router")
	}
}

func TestCompileAgent_OrchestratorTopology(t *testing.T) {
	spec := &stdlib.AgentSpec{
		Identity: stdlib.IdentitySpec{Core: "you coordinate"},
		SubAgents: []stdlib.SubAgentRef{
			{Name: "billing", RouteKey: "bill"},
			{Name: "support"},
		},
	}
	resolved := []string{}
	g, err := CompileAgent(spec, nilLLM{}, nilTools{}, CompileOpts{
		SubAgentResolver: func(name string) (*loom.Graph, error) {
			resolved = append(resolved, name)
			return loom.NewGraph("child:"+name, "noop"), nil
		},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	names := stepNames(g.Topology())
	for _, want := range []string{"prompt_assemble", "chat", "router", "sub_billing", "sub_support"} {
		if !names[want] {
			t.Fatalf("orchestrator topology missing %q: %v", want, names)
		}
	}
	if len(resolved) != 2 {
		t.Fatalf("expected resolver called for each sub-agent, got %v", resolved)
	}
}

// Without a resolver, sub_agents are inert — the agent stays a plain tool loop
// rather than failing to compile.
func TestCompileAgent_SubAgentsWithoutResolverStaysStandard(t *testing.T) {
	spec := &stdlib.AgentSpec{SubAgents: []stdlib.SubAgentRef{{Name: "billing"}}}
	g, err := CompileAgent(spec, nilLLM{}, nilTools{}, CompileOpts{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if stepNames(g.Topology())["router"] {
		t.Fatalf("no resolver → must not wire a router")
	}
}

func TestCompileAgent_NilSpec(t *testing.T) {
	if _, err := CompileAgent(nil, nilLLM{}, nilTools{}, CompileOpts{}); err == nil {
		t.Fatalf("nil spec must error")
	}
}

func TestCompileAgent_ResolverErrorPropagates(t *testing.T) {
	spec := &stdlib.AgentSpec{SubAgents: []stdlib.SubAgentRef{{Name: "billing"}}}
	_, err := CompileAgent(spec, nilLLM{}, nilTools{}, CompileOpts{
		SubAgentResolver: func(string) (*loom.Graph, error) { return nil, context.Canceled },
	})
	if err == nil {
		t.Fatalf("resolver error must propagate")
	}
}

func TestCompileAgent_ChatStepFactoryUsed(t *testing.T) {
	spec := &stdlib.AgentSpec{Identity: stdlib.IdentitySpec{Core: "x"}}
	used := false
	g, err := CompileAgent(spec, nilLLM{}, nilTools{}, CompileOpts{
		ChatStepFactory: func(_ contract.LLM, _ contract.ToolDispatcher, _ stdlib.ToolLoopOpts) loom.Step {
			return func(_ context.Context, _ loom.State) (loom.State, error) {
				used = true
				return loom.State{"output": "FROM_FACTORY"}, nil
			}
		},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res, err := g.Run(context.Background(), loom.State{
		"messages":          []contract.Message{{Role: "user", Content: "hi"}},
		"last_user_message": "hi",
	}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !used {
		t.Fatalf("ChatStepFactory step was not used")
	}
	if got := stdlib.GetString(res.State, "output", ""); got != "FROM_FACTORY" {
		t.Fatalf("output = %q, want FROM_FACTORY", got)
	}
}

// nil factory must keep the default tool-loop chat step (zero regression).
func TestCompileAgent_NilChatStepFactoryIsDefault(t *testing.T) {
	spec := &stdlib.AgentSpec{Identity: stdlib.IdentitySpec{Core: "x"}}
	g, err := CompileAgent(spec, nilLLM{}, nilTools{}, CompileOpts{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !stepNames(g.Topology())["chat"] {
		t.Fatalf("default build must still have a chat step")
	}
}

func TestSubAgentRouter_DeterministicRouting(t *testing.T) {
	router := buildSubAgentRouter([]stdlib.SubAgentRef{
		{Name: "billing", RouteKey: "bill"},
		{Name: "support"},
	})
	cases := map[string]string{
		"bill":        "sub_billing", // by RouteKey
		"sub_billing": "sub_billing", // by raw step name
		"support":     "sub_support", // RouteKey defaults to Name
		"unknown":     "",            // no match → halt
		"":            "",            // no delegation → halt
	}
	for delegate, want := range cases {
		next, err := router(context.Background(), loom.State{"__delegate_to": delegate})
		if err != nil {
			t.Fatalf("router(%q): %v", delegate, err)
		}
		if next != want {
			t.Fatalf("router(%q) = %q, want %q", delegate, next, want)
		}
	}
	// No __delegate_to key at all → halt.
	if next, _ := router(context.Background(), loom.State{}); next != "" {
		t.Fatalf("absent __delegate_to should halt, got %q", next)
	}
}

func TestLoopDetector_BlocksAtThreshold(t *testing.T) {
	d := newLoopDetector(3)
	h := d.hook()
	call := contract.ToolCall{ID: "1", Name: "search", Args: `{"q":"x"}`}
	// First 3 identical calls record cleanly; the hook blocks once the prior 3
	// all match.
	for i := 0; i < 3; i++ {
		if _, err := h.Pre(context.Background(), call); err != nil {
			t.Fatalf("call %d should not block yet: %v", i, err)
		}
		_ = h.Post(context.Background(), call, &contract.ToolResult{})
	}
	if _, err := h.Pre(context.Background(), call); err == nil {
		t.Fatalf("4th identical call must be blocked as a loop")
	}
}

func TestLoopDetector_DifferentCallsNoBlock(t *testing.T) {
	d := newLoopDetector(3)
	h := d.hook()
	for i := 0; i < 6; i++ {
		call := contract.ToolCall{ID: "1", Name: "search", Args: `{"q":"` + string(rune('a'+i)) + `"}`}
		if _, err := h.Pre(context.Background(), call); err != nil {
			t.Fatalf("distinct call %d must not block: %v", i, err)
		}
		_ = h.Post(context.Background(), call, &contract.ToolResult{})
	}
}
