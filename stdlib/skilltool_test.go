package stdlib_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

type skillToolInner struct {
	tools    []contract.ToolDef
	received []contract.ToolCall
	result   *contract.ToolResult
}

func (d *skillToolInner) ListTools(context.Context) ([]contract.ToolDef, error) {
	return d.tools, nil
}

func (d *skillToolInner) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	d.received = append(d.received, call)
	if d.result != nil {
		return d.result, nil
	}
	return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "inner result"}, nil
}

func TestSkillToolListed(t *testing.T) {
	inner := &skillToolInner{tools: []contract.ToolDef{{Name: "search", Description: "Search"}}}
	dispatcher := stdlib.NewSkillDispatcher(inner, []stdlib.SkillDef{{Name: "research"}})

	tools, err := dispatcher.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("ListTools returned %d tools, want 2", len(tools))
	}
	if tools[0].Name != "search" {
		t.Fatalf("first tool = %q, want inner tool search", tools[0].Name)
	}
	loadSkill := tools[1]
	if loadSkill.Name != "load_skill" {
		t.Fatalf("appended tool = %q, want load_skill", loadSkill.Name)
	}
	if !loadSkill.ReadOnly {
		t.Fatal("load_skill must be read-only")
	}
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(loadSkill.InputSchema, &schema); err != nil {
		t.Fatalf("unmarshal load_skill schema: %v", err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "name" {
		t.Fatalf("schema required = %v, want [name]", schema.Required)
	}
}

func TestLoadSkillHit(t *testing.T) {
	dispatcher := stdlib.NewSkillDispatcher(&skillToolInner{}, []stdlib.SkillDef{{
		Name:        "research",
		Description: "Find trustworthy sources.",
		Body:        "Read primary sources before answering.",
	}})

	result, err := dispatcher.Dispatch(context.Background(), contract.ToolCall{
		ID: "call-1", Name: "load_skill", Args: `{"name":"research"}`,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	want := "# skill: research\nFind trustworthy sources.\n\nRead primary sources before answering."
	if result.Content != want {
		t.Fatalf("Content = %q, want %q", result.Content, want)
	}
	if result.CallID != "call-1" || result.ToolName != "load_skill" || result.IsError {
		t.Fatalf("unexpected result metadata: %#v", result)
	}
}

func TestLoadSkillMiss(t *testing.T) {
	dispatcher := stdlib.NewSkillDispatcher(&skillToolInner{}, []stdlib.SkillDef{
		{Name: "research"},
		{Name: "writing"},
	})

	result, err := dispatcher.Dispatch(context.Background(), contract.ToolCall{
		ID: "call-2", Name: "load_skill", Args: `{"name":"missing"}`,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !result.IsError {
		t.Fatal("missing skill must return IsError=true")
	}
	for _, name := range []string{"research", "writing"} {
		if !strings.Contains(result.Content, name) {
			t.Errorf("missing-skill response %q does not list %q", result.Content, name)
		}
	}
}

func TestLoadSkillBadArgs(t *testing.T) {
	dispatcher := stdlib.NewSkillDispatcher(&skillToolInner{}, []stdlib.SkillDef{{Name: "research"}})

	result, err := dispatcher.Dispatch(context.Background(), contract.ToolCall{
		ID: "call-3", Name: "load_skill", Args: `{"name":`,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !result.IsError {
		t.Fatal("invalid JSON arguments must return IsError=true")
	}
	if !strings.Contains(result.Content, "name") {
		t.Fatalf("bad-args response %q must explain the required name argument", result.Content)
	}
}

func TestPassthrough(t *testing.T) {
	wantCall := contract.ToolCall{ID: "call-4", Name: "search", Args: `{"q":"loom"}`}
	wantResult := &contract.ToolResult{CallID: "call-4", ToolName: "search", Content: "found"}
	inner := &skillToolInner{result: wantResult}
	dispatcher := stdlib.NewSkillDispatcher(inner, []stdlib.SkillDef{{Name: "research"}})

	gotResult, err := dispatcher.Dispatch(context.Background(), wantCall)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(inner.received) != 1 || inner.received[0] != wantCall {
		t.Fatalf("inner received %#v, want unchanged call %#v", inner.received, wantCall)
	}
	if gotResult != wantResult {
		t.Fatal("passthrough must return the inner result unchanged")
	}
}

func TestEmptySkillsPassthrough(t *testing.T) {
	inner := &skillToolInner{tools: []contract.ToolDef{{Name: "search"}}}
	dispatcher := stdlib.NewSkillDispatcher(inner, nil)
	if dispatcher != inner {
		t.Fatal("empty skills must return the inner dispatcher itself")
	}

	tools, err := dispatcher.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "search" {
		t.Fatalf("ListTools = %#v, want only the inner search tool", tools)
	}
}

func TestInnerNameCollision(t *testing.T) {
	inner := &skillToolInner{tools: []contract.ToolDef{
		{Name: "load_skill", Description: "inner collision", ReadOnly: false},
		{Name: "search"},
	}}
	dispatcher := stdlib.NewSkillDispatcher(inner, []stdlib.SkillDef{{
		Name: "research", Description: "Research skill.", Body: "Research body.",
	}})

	tools, err := dispatcher.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	loadSkillCount := 0
	for _, tool := range tools {
		if tool.Name == "load_skill" {
			loadSkillCount++
			if !tool.ReadOnly || tool.Description == "inner collision" {
				t.Fatalf("collision retained inner definition: %#v", tool)
			}
		}
	}
	if loadSkillCount != 1 {
		t.Fatalf("load_skill appears %d times, want exactly once", loadSkillCount)
	}

	result, err := dispatcher.Dispatch(context.Background(), contract.ToolCall{
		ID: "call-5", Name: "load_skill", Args: `{"name":"research"}`,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(inner.received) != 0 {
		t.Fatalf("inner received collision call: %#v", inner.received)
	}
	if !strings.Contains(result.Content, "Research body.") {
		t.Fatalf("decorator did not handle collision: %q", result.Content)
	}
}

type skillToolScriptLLM struct {
	t     *testing.T
	calls int
}

func (m *skillToolScriptLLM) Chat(_ context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	m.t.Helper()
	switch m.calls {
	case 0:
		m.calls++
		found := false
		for _, tool := range req.Tools {
			if tool.Name == "load_skill" && tool.ReadOnly {
				found = true
			}
		}
		if !found {
			m.t.Fatal("first LLM request did not expose read-only load_skill")
		}
		return &contract.ChatResponse{ToolCalls: []contract.ToolCall{{
			ID: "load-1", Name: "load_skill", Args: `{"name":"research"}`,
		}}}, nil
	case 1:
		m.calls++
		if len(req.Messages) == 0 {
			m.t.Fatal("second LLM request has no messages")
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role != "tool" || last.ToolCallID != "load-1" {
			m.t.Fatalf("last message = %#v, want load_skill tool result", last)
		}
		if !strings.Contains(last.Content, "Research body from the running loop.") {
			m.t.Fatalf("tool result missing skill body: %q", last.Content)
		}
		return &contract.ChatResponse{Content: "finished with the loaded skill"}, nil
	default:
		m.t.Fatalf("unexpected LLM call %d", m.calls+1)
		return nil, nil
	}
}

func (m *skillToolScriptLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, fmt.Errorf("not implemented in skillToolScriptLLM")
}

func TestToolLoopIntegration(t *testing.T) {
	llm := &skillToolScriptLLM{t: t}
	tools := stdlib.NewSkillDispatcher(&skillToolInner{}, []stdlib.SkillDef{{
		Name:        "research",
		Description: "Use reliable evidence.",
		Body:        "Research body from the running loop.",
	}})
	graph := loom.NewGraph("skill-tool-integration", "chat")
	graph.AddStep("chat", stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test", MaxIterations: 3,
	}), loom.End())

	result, err := graph.Run(context.Background(), loom.State{
		"messages": []contract.Message{{Role: "user", Content: "Use the research skill."}},
	}, nil)
	if err != nil {
		t.Fatalf("Graph.Run: %v", err)
	}
	if result.State["output"] != "finished with the loaded skill" {
		t.Fatalf("output = %v", result.State["output"])
	}
	if llm.calls != 2 {
		t.Fatalf("LLM calls = %d, want 2", llm.calls)
	}
}
