// Package compiler turns a stdlib.AgentSpec into a runnable loom.Graph for
// deterministic orchestration: a parent agent routes to named sub-agents by
// writing a RouteKey into state, instead of relying on the model to "decide the
// next step" in a flat tool loop.
//
// This is the lean, spec-driven port of the archived weave compiler. It
// deliberately sheds the platform-shell coupling (registry.AgentRecord) and the
// orthogonal extras that the loom consolidation cut or deferred — memory
// retrieval / auto-remember (replaced by weave's slot system), otel
// trace/audit, llmrouter fallback, guards, compaction, semantic (embedder)
// skill matching. What remains is the orchestration topology + deterministic
// router. See plans/loom-p3-orchestration.md.
package compiler

import (
	"fmt"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

// CompileOpts carries the runtime parameters the spec does not (the LLM/model,
// tools, and the resolver that turns a sub-agent name into a compiled child
// graph). All fields are optional except SubAgentResolver when the spec
// declares sub-agents.
type CompileOpts struct {
	Name             string                              // graph display name (default "agent")
	Model            string                              // model id (default "deepseek-chat")
	Profile          string                              // active profile name
	Effort           contract.EffortLevel                // default EffortMedium
	Context          map[string]any                      // runtime context → "## 当前上下文"
	ToolHooks        []contract.ToolHook                 // injected tool pre/post hooks
	MaxToolRepeats   int                                 // >0 wires a loop detector on the chat step
	Store            loom.Store                          // for sub-graph checkpointing
	SubAgentResolver func(name string) (*loom.Graph, error) // resolve a sub-agent name → child graph
}

// CompileAgent converts an AgentSpec into a runnable loom Graph.
//
//	No sub-agents (default):
//	    prompt_assemble → chat → END
//	Orchestrator (sub_agents declared + resolver provided):
//	    prompt_assemble → chat → route → sub_<name> → END
//
// The route is a deterministic Branch: the chat step (or a tool) writes
// __delegate_to into state, and the router maps that value (or a sub-agent's
// RouteKey) to the matching sub_<name> step.
func CompileAgent(spec *stdlib.AgentSpec, llm contract.LLM, tools contract.ToolDispatcher, opts CompileOpts) (*loom.Graph, error) {
	if spec == nil {
		return nil, fmt.Errorf("compiler: nil AgentSpec")
	}
	model := opts.Model
	if model == "" {
		model = "deepseek-chat"
	}
	effort := opts.Effort
	if effort == "" {
		effort = contract.EffortMedium
	}
	name := opts.Name
	if name == "" {
		name = "agent"
	}

	hasSubAgents := len(spec.SubAgents) > 0 && opts.SubAgentResolver != nil

	graphOpts := []loom.GraphOption{loom.WithMaxIterations(50)}
	// MergeConfig so messages accumulate correctly across sub-graphs.
	if hasSubAgents {
		graphOpts = append(graphOpts, loom.WithMergeConfig(loom.DefaultMergeConfig()))
	}

	g := loom.NewGraph(name, "prompt_assemble", graphOpts...)

	// Identity: SystemPrompt (v1.2 compat) folds into core; always-active skill
	// bodies are injected directly; the rest go through the prompt assembler's
	// matcher (KeywordMatcher — semantic/embedder matching is not ported).
	identity := spec.Identity
	if identity.Core == "" && spec.SystemPrompt != "" {
		identity.Core = spec.SystemPrompt
	}
	var matchableSkills []stdlib.SkillDef
	for _, s := range spec.Skills {
		if s.AlwaysActive && s.Body != "" {
			identity.Core += "\n\n## 技能: " + s.Name + "\n以下规则必须遵守：\n" + s.Body
		} else {
			matchableSkills = append(matchableSkills, s)
		}
	}

	// --- Step: prompt assembly → chat ---
	g.AddStep("prompt_assemble", stdlib.NewPromptAssembleStep(stdlib.PromptConfig{
		Identity:              identity,
		Skills:                matchableSkills,
		Profile:               opts.Profile,
		ProfilesMap:           spec.Profiles,
		Context:               opts.Context,
		MaxSystemPromptTokens: 8000,
		SkillMatcher:          &stdlib.KeywordMatcher{},
	}), loom.Always("chat"))

	// --- Step: chat (tool loop) ---
	toolHooks := append([]contract.ToolHook{}, opts.ToolHooks...)
	if opts.MaxToolRepeats > 0 {
		toolHooks = append(toolHooks, newLoopDetector(opts.MaxToolRepeats).hook())
	}
	chatRouter := loom.End()
	if hasSubAgents {
		chatRouter = buildSubAgentRouter(spec.SubAgents)
	}
	g.AddStep("chat", stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model:         model,
		SystemPrompt:  spec.SystemPrompt,
		MaxIterations: 20,
		Effort:        effort,
		ToolHooks:     toolHooks,
	}), chatRouter)

	// --- Steps: sub-agent delegation (optional) ---
	if hasSubAgents {
		for _, sa := range spec.SubAgents {
			child, err := opts.SubAgentResolver(sa.Name)
			if err != nil {
				return nil, fmt.Errorf("sub-agent %q: %w", sa.Name, err)
			}
			g.AddStep("sub_"+sa.Name, stdlib.NewSubGraphStep(child, opts.Store, stdlib.SubGraphOpts{
				YieldPolicy: stdlib.YieldBubble,
			}), loom.End())
		}
	}

	g.SetTopology(buildTopology(model, spec.SubAgents, hasSubAgents))
	return g, nil
}

// buildSubAgentRouter creates a deterministic Branch router. The chat step (or a
// tool) signals delegation by writing __delegate_to into state; the router maps
// it to the matching sub_<name> step. A sub-agent's RouteKey (default: its Name)
// and its raw step name are both accepted.
func buildSubAgentRouter(subAgents []stdlib.SubAgentRef) loom.Router {
	routes := make(map[string]string)
	for _, sa := range subAgents {
		key := sa.RouteKey
		if key == "" {
			key = sa.Name
		}
		stepName := "sub_" + sa.Name
		routes[key] = stepName
		if key != stepName {
			routes[stepName] = stepName
		}
	}
	return loom.BranchFunc(
		func(s loom.State) string {
			if delegate, ok := s["__delegate_to"].(string); ok && delegate != "" {
				return delegate
			}
			return ""
		},
		routes,
		"", // fallback: end graph (no delegation)
	)
}

// buildTopology declares the graph shape for visualization / introspection.
func buildTopology(model string, subAgents []stdlib.SubAgentRef, hasSubAgents bool) []loom.StepInfo {
	chatNext := ""
	if hasSubAgents {
		chatNext = "router"
	}
	topo := []loom.StepInfo{
		{Name: "prompt_assemble", Edges: []loom.Edge{{To: "chat"}}},
		{Name: "chat", Detail: model, Edges: []loom.Edge{{To: chatNext}}},
	}
	if hasSubAgents {
		var branches []loom.Edge
		for _, sa := range subAgents {
			key := sa.RouteKey
			if key == "" {
				key = sa.Name
			}
			branches = append(branches, loom.Edge{To: "sub_" + sa.Name, Label: key})
		}
		branches = append(branches, loom.Edge{To: "", Label: "no delegation"})
		topo = append(topo, loom.StepInfo{Name: "router", Edges: branches})
		for _, sa := range subAgents {
			topo = append(topo, loom.StepInfo{Name: "sub_" + sa.Name, Detail: sa.Name, Edges: []loom.Edge{{To: ""}}})
		}
	}
	return topo
}
