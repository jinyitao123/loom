package main

import (
	"fmt"           // 为"子 agent 配置缺失"构造带路径的错误
	"path/filepath" // 拼接 .loom-agents/<name>.json 的寻址路径

	"github.com/jinyitao123/loom"                 // Graph：子 agent 编译产物（图）的类型
	"github.com/jinyitao123/loom/contract"        // LLM / ToolDispatcher / ToolHook 等引擎契约
	"github.com/jinyitao123/loom/stdlib"          // AgentSpec 的定义方
	"github.com/jinyitao123/loom/stdlib/compiler" // 把 AgentSpec 编译成可执行图
)

// loadChildSpec loads a sub-agent's spec from <cwd>/.loom-agents/<name>.json (a
// serialized stdlib.AgentSpec the host writes, mirroring .loom-agent.json).
// Unlike the parent spec, a declared-but-missing child is a loud error — better
// to fail the run than silently drop a sub-agent the parent expects.
// 中文补充：从 <cwd>/.loom-agents/<name>.json 装载子 agent 的 spec
// （由宿主写入，格式与父级的 .loom-agent.json 一致）。与父级"缺文件不算错"
// 的约定相反：父级已声明的子 agent 若找不到 spec，必须响亮报错——
// 宁可让本轮运行失败，也不能静默丢掉父级预期存在的子 agent。
func loadChildSpec(cwd, name string) (*stdlib.AgentSpec, error) {
	// 子 agent 配置的固定寻址约定：<cwd>/.loom-agents/<name>.json。
	path := filepath.Join(cwd, ".loom-agents", name+".json")
	// 复用父级的装载逻辑（读文件 + 解析 JSON）。
	spec, err := loadAgentSpec(path)
	if err != nil {
		return nil, err
	}
	// loadAgentSpec 对"文件不存在"返回 (nil, nil)；对子 agent 而言这是硬错误。
	if spec == nil {
		return nil, fmt.Errorf("spec not found at %s", path)
	}
	return spec, nil // 装载成功
}

// cliSubAgentResolver returns a resolver that compiles a sub-agent name into a
// LEAF child graph (prompt_assemble → chat → END). The child is compiled with
// SubAgentResolver=nil — single-hop delegation only, which also guarantees no
// recursion even if a child spec itself declares sub_agents. It shares the
// parent's streaming llm (so child tokens stream) and tool-event hook (so child
// tool calls emit), and uses the REAL tools (children don't re-delegate in v1).
// 中文补充：返回一个子 agent 解析器——把子 agent 名字编译成"叶子"子图
// （prompt_assemble → chat → END）。关键取舍：编译时 SubAgentResolver 传 nil，
// 只允许单跳委派；即使子 spec 自己声明了 sub_agents 也不会递归展开。
// 子图复用父级的流式 LLM（子 agent 的 token 同样实时流出）和工具事件钩子
// （子 agent 的工具调用同样上报事件），并直接使用真实工具集
// （v1 约定：子 agent 不再二次委派）。
func cliSubAgentResolver(cfg runConfig, llm contract.LLM, tools contract.ToolDispatcher, out sink) func(string) (*loom.Graph, error) {
	// 返回闭包：捕获运行配置、LLM、工具分发器与事件输出端，按名字惰性编译子图。
	return func(name string) (*loom.Graph, error) {
		// 先装载子 agent 的 spec；缺失即失败（见 loadChildSpec 的约定）。
		childSpec, err := loadChildSpec(cfg.cwd, name)
		if err != nil {
			return nil, err
		}
		// 把 spec 编译成可执行图：沿用父级的 model/profile，注入工具事件钩子，
		// 让子 agent 的一举一动对宿主完全可观测。
		return compiler.CompileAgent(childSpec, llm, tools, compiler.CompileOpts{
			Name:             name,                                    // 子 agent 名（也用于事件归属标识）
			Model:            cfg.model,                               // 沿用父级选定的模型
			Profile:          cfg.profile,                             // 沿用父级的 profile
			ToolHooks:        []contract.ToolHook{toolEventHook(out)}, // 子 agent 的工具调用同样发事件到 NDJSON 流
			SubAgentResolver: nil,                                     // leaf — no recursion, single-hop only
			// 中文补充：nil 表示叶子节点——子图内禁止再解析子 agent，从结构上杜绝递归。
		})
	}
}
