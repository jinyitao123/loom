package main

import (
	"encoding/json" // 反序列化 AgentSpec 的 JSON 配置
	"errors"        // errors.Is：判定"文件不存在"这类哨兵错误
	"fmt"           // 为解析失败包装带路径的错误信息
	"io/fs"         // fs.ErrNotExist：跨平台的"文件不存在"哨兵
	"os"            // 读取配置文件内容

	"github.com/jinyitao123/loom/stdlib" // AgentSpec / IdentitySpec 的定义方
)

// loadAgentSpec reads --agent-config: a serialized stdlib.AgentSpec the weave
// daemon compiles from the agent's rich config (system prompt / identity /
// skills / profiles). A missing path/file is not an error — the agent simply
// runs with no rich config.
// 中文补充：装载 --agent-config 指向的 AgentSpec（由 weave 守护进程把
// agent 的富配置——系统提示词/identity/技能/profile——编译后写出的 JSON）。
// 装载约定："没有配置"是合法状态：路径为空或文件不存在都返回 (nil, nil)，
// agent 以无富配置模式运行；只有文件存在却读不了/解析不了才算错误。
func loadAgentSpec(path string) (*stdlib.AgentSpec, error) {
	// 未传 --agent-config：按"无配置"处理，不是错误。
	if path == "" {
		return nil, nil
	}
	// #nosec G304 -- path is the operator/daemon-supplied .loom-agent.json in the
	// agent workdir, not untrusted remote input.
	// 中文补充：路径由运维方/守护进程提供（agent 工作目录下的 .loom-agent.json），
	// 并非不可信的远端输入，因此可以放行 G304（任意文件读取）告警。
	data, err := os.ReadFile(path)
	// 文件不存在同样按"无配置"处理——宿主可能尚未写出该文件。
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	// 其他读取错误（权限、IO 故障等）如实上报。
	if err != nil {
		return nil, err
	}
	// 反序列化为 AgentSpec；解析失败时把文件路径包进错误里，便于定位坏配置。
	var spec stdlib.AgentSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse agent-config %s: %w", path, err)
	}
	return &spec, nil // 成功装载：返回解析后的 spec
}

// specIdentity resolves the identity used for prompt assembly, falling back to
// the flat SystemPrompt (v1.2 compat) when no tiered identity is set.
// 中文补充：解析 prompt 组装所用的 identity。兼容策略：若分层 identity
// （Core/Extended/Raw）全部为空、而旧版扁平字段 SystemPrompt 非空，
// 就把 SystemPrompt 提升为 Core，保证 v1.2 老配置依然生效。
func specIdentity(s *stdlib.AgentSpec) stdlib.IdentitySpec {
	// 先做值拷贝，避免回退逻辑改动原 spec 上的 Identity。
	id := s.Identity
	// 分层 identity 完全为空且存在旧式 SystemPrompt：回退到扁平写法（向后兼容）。
	if id.Core == "" && id.Extended == "" && id.Raw == "" && s.SystemPrompt != "" {
		id.Core = s.SystemPrompt
	}
	return id // 返回最终用于组装 prompt 的 identity
}
