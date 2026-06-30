package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

// delegateToolName is the synthetic tool the model calls to hand this turn off
// to a specialist sub-agent.
const delegateToolName = "delegate"

// delegateSignal records the sub-agent a delegate tool call chose. It is shared
// between the delegateDispatcher (which captures the call) and the chat-step
// wrapper (which turns the choice into a router signal). A tool result cannot
// write graph state, so this closure-shared struct is how the model's routing
// decision reaches the deterministic router. Mutex-guarded because read-only
// tool calls can dispatch in parallel.
type delegateSignal struct {
	mu    sync.Mutex
	set   bool
	agent string
	task  string
}

func (s *delegateSignal) record(agent, task string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set = true
	s.agent = agent
	s.task = task
}

func (s *delegateSignal) read() (set bool, agent, task string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.set, s.agent, s.task
}

// delegateDispatcher wraps the real tool dispatcher and adds one synthetic
// `delegate` tool. When the model calls it, the chosen sub-agent is recorded in
// the shared signal and a benign result is returned; every other tool passes
// through to the inner dispatcher.
type delegateDispatcher struct {
	inner contract.ToolDispatcher
	subs  []stdlib.SubAgentRef
	sig   *delegateSignal
}

func newDelegateDispatcher(inner contract.ToolDispatcher, subs []stdlib.SubAgentRef, sig *delegateSignal) *delegateDispatcher {
	return &delegateDispatcher{inner: inner, subs: subs, sig: sig}
}

// routeKey returns the value the router branches on for a sub-agent (RouteKey,
// defaulting to Name).
func routeKey(sa stdlib.SubAgentRef) string {
	if sa.RouteKey != "" {
		return sa.RouteKey
	}
	return sa.Name
}

func (d *delegateDispatcher) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	var tools []contract.ToolDef
	if d.inner != nil {
		inner, err := d.inner.ListTools(ctx)
		if err != nil {
			return nil, err
		}
		tools = append(tools, inner...)
	}
	tools = append(tools, d.delegateToolDef())
	return tools, nil
}

func (d *delegateDispatcher) delegateToolDef() contract.ToolDef {
	names := make([]string, 0, len(d.subs))
	lines := make([]string, 0, len(d.subs))
	for _, sa := range d.subs {
		key := routeKey(sa)
		names = append(names, key)
		desc := sa.Description
		if desc == "" {
			desc = sa.Name
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", key, desc))
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"agent": map[string]any{"type": "string", "enum": names, "description": "which sub-agent to hand off to"},
			"task":  map[string]any{"type": "string", "description": "the task / instruction for the sub-agent"},
		},
		"required": []string{"agent", "task"},
	}
	raw, _ := json.Marshal(schema)
	return contract.ToolDef{
		Name:        delegateToolName,
		Description: "Hand this turn off to a specialist sub-agent when the request matches one of them. Sub-agents:\n" + strings.Join(lines, "\n"),
		InputSchema: raw,
	}
}

func (d *delegateDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if call.Name != delegateToolName {
		if d.inner != nil {
			return d.inner.Dispatch(ctx, call)
		}
		return &contract.ToolResult{CallID: call.ID, Content: "no tools configured", IsError: true}, nil
	}
	var args struct {
		Agent string `json:"agent"`
		Task  string `json:"task"`
	}
	if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
		return &contract.ToolResult{CallID: call.ID, Content: "delegate: invalid args", IsError: true}, nil
	}
	if !d.known(args.Agent) {
		return &contract.ToolResult{CallID: call.ID, Content: fmt.Sprintf("delegate: unknown sub-agent %q", args.Agent), IsError: true}, nil
	}
	d.sig.record(args.Agent, args.Task)
	return &contract.ToolResult{CallID: call.ID, Content: "delegating to " + args.Agent}, nil
}

func (d *delegateDispatcher) known(name string) bool {
	for _, sa := range d.subs {
		if routeKey(sa) == name || sa.Name == name {
			return true
		}
	}
	return false
}

// delegateChatStepFactory builds the orchestrator's chat step: run the normal
// tool loop, then — if the model called delegate — return a state carrying
// __delegate_to (and the delegated task as last_user_message) so the compiler's
// deterministic router branches to sub_<agent>. Otherwise return the tool loop's
// own delta (the answer). Wire it via compiler.CompileOpts.ChatStepFactory.
func delegateChatStepFactory(sig *delegateSignal) func(contract.LLM, contract.ToolDispatcher, stdlib.ToolLoopOpts) loom.Step {
	return func(llm contract.LLM, tools contract.ToolDispatcher, opts stdlib.ToolLoopOpts) loom.Step {
		inner := stdlib.NewToolLoopStep(llm, tools, opts)
		return func(ctx context.Context, state loom.State) (loom.State, error) {
			delta, err := inner(ctx, state)
			if err != nil {
				return delta, err
			}
			if set, agent, task := sig.read(); set {
				// Delegation is terminal for this turn: drop any pre-delegation
				// output — the sub-agent owns the answer.
				next := loom.State{"__delegate_to": agent}
				if task != "" {
					next["last_user_message"] = task
				}
				return next, nil
			}
			return delta, nil
		}
	}
}
