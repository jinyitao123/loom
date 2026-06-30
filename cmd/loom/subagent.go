package main

import (
	"fmt"
	"path/filepath"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/loom/stdlib/compiler"
)

// loadChildSpec loads a sub-agent's spec from <cwd>/.loom-agents/<name>.json (a
// serialized stdlib.AgentSpec the host writes, mirroring .loom-agent.json).
// Unlike the parent spec, a declared-but-missing child is a loud error — better
// to fail the run than silently drop a sub-agent the parent expects.
func loadChildSpec(cwd, name string) (*stdlib.AgentSpec, error) {
	path := filepath.Join(cwd, ".loom-agents", name+".json")
	spec, err := loadAgentSpec(path)
	if err != nil {
		return nil, err
	}
	if spec == nil {
		return nil, fmt.Errorf("spec not found at %s", path)
	}
	return spec, nil
}

// cliSubAgentResolver returns a resolver that compiles a sub-agent name into a
// LEAF child graph (prompt_assemble → chat → END). The child is compiled with
// SubAgentResolver=nil — single-hop delegation only, which also guarantees no
// recursion even if a child spec itself declares sub_agents. It shares the
// parent's streaming llm (so child tokens stream) and tool-event hook (so child
// tool calls emit), and uses the REAL tools (children don't re-delegate in v1).
func cliSubAgentResolver(cfg runConfig, llm contract.LLM, tools contract.ToolDispatcher, out sink) func(string) (*loom.Graph, error) {
	return func(name string) (*loom.Graph, error) {
		childSpec, err := loadChildSpec(cfg.cwd, name)
		if err != nil {
			return nil, err
		}
		return compiler.CompileAgent(childSpec, llm, tools, compiler.CompileOpts{
			Name:             name,
			Model:            cfg.model,
			Profile:          cfg.profile,
			ToolHooks:        []contract.ToolHook{toolEventHook(out)},
			SubAgentResolver: nil, // leaf — no recursion, single-hop only
		})
	}
}
