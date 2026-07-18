package stdlib

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// IdentitySpec holds tiered identity content (v1.3).
// Core is always loaded; Extended is loaded when budget allows.
// IdentitySpec 是分层身份：身份文件按第一个 "## " 二级标题切成两档——
// 标题前是核心身份（永远注入系统提示词），标题后是扩展身份（token 预算允许才注入）。
type IdentitySpec struct {
	Core     string `json:"core,omitempty"`     // text before first ## heading — always in prompt
	Extended string `json:"extended,omitempty"` // text after first ## heading — loaded when budget allows
	Raw      string `json:"raw,omitempty"`      // full original content
	// Core：第一个 ## 之前的文本，agent 人格的底线，永远进入提示词。
	// Extended：第一个 ## 之后的全部文本，预算装得下才注入。
	// Raw：未切分的完整原文，供 v1.2 兼容字段 SystemPrompt 使用。
}

// ProfileEntry defines per-role behavior: system prompt addition and optional greeting.
// ProfileEntry 定义单个 Profile（角色档位）的行为差异：系统提示词补充 + 可选的开场白。
type ProfileEntry struct {
	// SystemAddition is appended to the system prompt when this profile is active.
	// For backward compatibility, if a profile value is a plain string in JSON,
	// it is treated as SystemAddition.
	// SystemAddition：该 Profile 激活时叠加到系统提示词后面的文本；
	// 向后兼容约定——JSON 中 Profile 的值若是裸字符串，整体按 SystemAddition 处理。
	SystemAddition string `json:"system_addition,omitempty"`
	// Greeting is the initial message sent when a new session starts with this profile.
	// The application reads this from the agent config and sends it as the first message.
	// Greeting：以该 Profile 开启新会话时的开场白；由应用层读取并作为首条消息发送，内核不直接使用。
	Greeting string `json:"greeting,omitempty"`
}

// AgentSpec is the unified agent specification loaded from files or constructed programmatically.
// AgentSpec 是统一的 agent 规格：既可以从目录按约定装载（见 LoadFromDirectory），
// 也可以在代码里直接构造，是"文件系统约定 → 可执行 agent"之间的中间表示。
type AgentSpec struct {
	SystemPrompt string `json:"system_prompt,omitempty"` // v1.2 compat — equals Identity.Raw
	// SystemPrompt：v1.2 兼容字段，恒等于 Identity.Raw，供尚未迁移到分层身份的调用方使用。
	Identity IdentitySpec `json:"identity,omitempty"` // v1.3: tiered identity
	// Identity：v1.3 分层身份（核心/扩展两档），由身份文件按第一个 ## 切分而来。
	Profiles    map[string]ProfileEntry `json:"profiles,omitempty"`     // Profile 名 → 行为条目，来自 PROFILES.md 的 ## 分节
	Skills      []SkillDef              `json:"skills,omitempty"`       // 技能列表，来自 skills/ 子目录逐个装载
	HealthCheck *HealthCheckDef         `json:"health_check,omitempty"` // 心跳/健康检查配置，来自 HEARTBEAT.md，可为 nil
	// SubAgents declares deterministic orchestration: the agent routes to a
	// named sub-agent by writing its RouteKey into state. Empty = no
	// orchestration → the run uses the plain tool loop (back-compat). (P3)
	// SubAgents 声明确定性编排：父 agent 把某个子 agent 的 RouteKey 写入状态，
	// 路由器即把控制权转交给它；留空表示不编排，运行时退回纯工具循环（向后兼容）。
	SubAgents []SubAgentRef `json:"sub_agents,omitempty"`
	// GraphType selects the compiled topology. "" / "standard" = the default
	// (prompt → chat → [route → sub-agents]); other values are reserved for
	// registered custom factories. (P3)
	// GraphType 选择编译出的图拓扑：""/"standard" 为默认拓扑（提示词组装 → 对话 → [路由 → 子 agent]）；
	// 其他取值是自定义拓扑工厂的扩展点——运行时按名字查已注册的工厂来构图。
	GraphType string `json:"graph_type,omitempty"`
}

// SubAgentRef references another agent for deterministic orchestration. The
// parent routes to it when its RouteKey appears in state (set by the model via
// __delegate_to, or by a step). (P3)
// SubAgentRef 引用一个子 agent：当状态中出现它的 RouteKey（由模型经 __delegate_to 写入，
// 或由某个步骤直接写入）时路由过去——路由决策是确定性的查表，不靠模型自由发挥。
type SubAgentRef struct {
	Name        string `json:"name"`                  // 子 agent 名称（唯一标识）
	Description string `json:"description,omitempty"` // when to route here (for the model)
	// Description：写给模型看的"什么情况应路由到此"的说明。
	RouteKey string `json:"route_key,omitempty"` // state value that routes here (defaults to Name)
	// RouteKey：触发路由的状态值；缺省时回退为 Name。
}

// SkillDef represents a skill loaded from a SKILL.md file.
// SkillDef 表示从 SKILL.md 装载的一个技能：元信息（名称/描述）用于索引与匹配，
// 正文按需注入提示词，附带的脚本与参考资料以文件路径形式暴露给运行时。
type SkillDef struct {
	Name         string `json:"name"`                    // 技能名，取自 frontmatter 的 name 字段
	Description  string `json:"description"`             // 技能描述，取自 frontmatter 的 description 字段，参与关键词匹配
	Body         string `json:"body,omitempty"`          // frontmatter 之后的正文，即技能的具体指令
	AlwaysActive bool   `json:"always_active,omitempty"` // if true, body is always injected (no matching needed)
	// AlwaysActive 为 true 时正文无条件注入提示词，绕过技能匹配环节。
	Scripts    []string `json:"scripts,omitempty"`    // scripts/ 子目录下收集到的脚本文件路径
	References []string `json:"references,omitempty"` // references/ 子目录下收集到的参考资料路径
}

// HealthCheckDef holds heartbeat/health-check configuration.
// HealthCheckDef 承载心跳/健康检查的配置内容（即 HEARTBEAT.md 的原文）。
type HealthCheckDef struct {
	Content string `json:"content,omitempty"` // HEARTBEAT.md 的完整文本
}

// splitTiered splits content at the first ## heading into Core (before) and Extended (after).
// splitTiered 实现身份文件的"两档切分"约定：以第一个 "## " 二级标题为界，
// 标题之前为核心身份 Core，标题（含标题本身）之后整体为扩展身份 Extended。
func splitTiered(content string) (core, extended string) {
	// 借助前导换行符 "\n## " 定位"行首的 ##"，避免误匹配行中出现的 "## "。
	idx := strings.Index(content, "\n## ")
	if idx < 0 {
		// Also check if it starts with ##
		// 边界：文件第一行就是 ## 标题——此时没有核心部分，全文都归入扩展档。
		if strings.HasPrefix(content, "## ") {
			return "", content
		}
		// 通篇没有 ## 标题：全文都是核心身份，扩展档为空。
		return strings.TrimSpace(content), ""
	}
	// 正常切分：界标前是 Core；idx+1 跳过换行符本身，从 "## " 行起都是 Extended。
	return strings.TrimSpace(content[:idx]), strings.TrimSpace(content[idx+1:])
}

// LoadFromDirectory loads an AgentSpec from a directory using priority-based file detection.
//
// Priority:
//  1. CLAUDE.md — if present, becomes Identity (overrides SOUL+IDENTITY)
//  2. AGENTS.md — same as CLAUDE.md (OpenAI convention)
//  3. SOUL.md + IDENTITY.md — SOUL → Core, IDENTITY → Extended
//
// LoadFromDirectory 按"目录即 agent"的约定装载 AgentSpec：
// 身份文件按优先级探测（CLAUDE.md > AGENTS.md > SOUL.md+IDENTITY.md，高优先级存在即忽略其余），
// 另外装载 PROFILES.md（Profile 覆盖层）、HEARTBEAT.md（健康检查）与 skills/ 子目录（技能）。
// 所有文件都是可选的：缺失按空处理，但真实的读盘错误会让装载失败。
func LoadFromDirectory(dir string) (*AgentSpec, error) {
	// 预置空 Profiles map，保证调用方拿到的永远是可用的 map 而非 nil。
	spec := &AgentSpec{
		Profiles: make(map[string]ProfileEntry),
	}

	// Priority 1: CLAUDE.md overrides everything.
	// 优先级 1：CLAUDE.md 存在则独占身份来源，后面的 AGENTS/SOUL/IDENTITY 全部忽略。
	if content, err := readFileIfExists(filepath.Join(dir, "CLAUDE.md")); err != nil {
		return nil, err // 读盘错误（非"文件不存在"）直接失败
	} else if content != "" {
		// 按第一个 ## 切成核心/扩展两档；Raw 保留完整原文。
		core, extended := splitTiered(content)
		spec.Identity = IdentitySpec{Core: core, Extended: extended, Raw: content}
		spec.SystemPrompt = content // v1.2 兼容字段与原文保持一致
	} else if content, err := readFileIfExists(filepath.Join(dir, "AGENTS.md")); err != nil {
		// 优先级 2：AGENTS.md（OpenAI 生态的同类约定）；走到这里说明 CLAUDE.md 不存在。
		return nil, err
	} else if content != "" {
		// AGENTS.md 的处理方式与 CLAUDE.md 完全一致：两档切分 + 同步兼容字段。
		core, extended := splitTiered(content)
		spec.Identity = IdentitySpec{Core: core, Extended: extended, Raw: content}
		spec.SystemPrompt = content
	} else {
		// Priority 3: SOUL.md + IDENTITY.md
		// 优先级 3：SOUL.md 与 IDENTITY.md 组合——SOUL 提供核心、IDENTITY 补充扩展。
		var rawParts []string // 用于拼出 Raw 的原文片段
		// 两个文件都允许缺失（返回空串），但真实读盘错误必须上抛。
		soul, err := readFileIfExists(filepath.Join(dir, "SOUL.md"))
		if err != nil {
			return nil, err
		}
		identity, err := readFileIfExists(filepath.Join(dir, "IDENTITY.md"))
		if err != nil {
			return nil, err
		}

		// SOUL.md: split into Core/Extended at first ##
		// SOUL.md 自身同样按第一个 ## 切分：标题前的部分成为核心身份。
		soulCore, soulExtended := splitTiered(soul)
		spec.Identity.Core = soulCore

		// Combine: SOUL's extended parts + IDENTITY.md → Extended
		// 扩展身份 = SOUL 的扩展段 + IDENTITY.md 全文，非空片段之间用空行拼接。
		var extParts []string
		if soulExtended != "" {
			extParts = append(extParts, soulExtended)
		}
		if identity != "" {
			extParts = append(extParts, strings.TrimSpace(identity))
		}
		spec.Identity.Extended = strings.Join(extParts, "\n\n")

		// Raw 保留两个文件的完整原文拼接（只收非空者），SystemPrompt 与之保持一致。
		if soul != "" {
			rawParts = append(rawParts, soul)
		}
		if identity != "" {
			rawParts = append(rawParts, identity)
		}
		spec.Identity.Raw = strings.Join(rawParts, "\n\n")
		spec.SystemPrompt = spec.Identity.Raw
	}

	// PROFILES.md — sections delimited by ## headers.
	// PROFILES.md：每个 ## 分节即一个 Profile——节标题是 Profile 名，节内容是 SystemAddition。
	if content, err := readFileIfExists(filepath.Join(dir, "PROFILES.md")); err != nil {
		return nil, err
	} else if content != "" {
		spec.Profiles = parseProfileSections(content)
	}

	// HEARTBEAT.md
	// HEARTBEAT.md：文件存在即把全文作为健康检查配置。
	if content, err := readFileIfExists(filepath.Join(dir, "HEARTBEAT.md")); err != nil {
		return nil, err
	} else if content != "" {
		spec.HealthCheck = &HealthCheckDef{Content: content}
	}

	// skills/ subdirectories.
	// skills/ 目录约定：每个子目录一个技能（内含 SKILL.md）；
	// skills/ 不存在时 ReadDir 返回错误，这里按"没有技能"静默跳过。
	skillsDir := filepath.Join(dir, "skills")
	if entries, err := os.ReadDir(skillsDir); err == nil {
		for _, entry := range entries {
			// 忽略散落在 skills/ 下的普通文件，只认子目录。
			if !entry.IsDir() {
				continue
			}
			skill, err := loadSkill(filepath.Join(skillsDir, entry.Name()))
			if err != nil {
				return nil, err // 单个技能解析出错视为整体装载失败，宁缺毋滥
			}
			// loadSkill 对缺少 SKILL.md 的目录返回 (nil, nil)，静默跳过。
			if skill != nil {
				spec.Skills = append(spec.Skills, *skill)
			}
		}
	}

	return spec, nil
}

// loadSkill loads a SkillDef from a skill directory.
// loadSkill 从单个技能目录装载 SkillDef：解析 SKILL.md 的 frontmatter 与正文，
// 并收集 scripts/、references/ 两个子目录下的附件文件路径。
func loadSkill(dir string) (*SkillDef, error) {
	// 技能目录的入口文件固定为 SKILL.md。
	skillFile := filepath.Join(dir, "SKILL.md")
	content, err := readFileIfExists(skillFile)
	if err != nil {
		return nil, err
	}
	// 没有 SKILL.md 的目录不算技能：返回 (nil, nil) 让调用方静默跳过而非报错。
	if content == "" {
		return nil, nil
	}

	skill := &SkillDef{}
	// 切出 frontmatter（元信息）与正文；正文去掉首尾空白后即技能指令本体。
	frontmatter, body := parseFrontmatter(content)
	skill.Body = strings.TrimSpace(body)

	// frontmatter 只消费两个键：name（技能名）与 description（描述，参与匹配），其余键忽略。
	if name, ok := frontmatter["name"]; ok {
		skill.Name = name
	}
	if desc, ok := frontmatter["description"]; ok {
		skill.Description = desc
	}

	// Collect scripts.
	// 收集 scripts/ 下的所有普通文件路径（不递归子目录）；目录不存在时 ReadDir 出错，静默跳过。
	scriptsDir := filepath.Join(dir, "scripts")
	if entries, err := os.ReadDir(scriptsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				skill.Scripts = append(skill.Scripts, filepath.Join(scriptsDir, e.Name()))
			}
		}
	}

	// Collect references.
	// 同理收集 references/ 下的参考资料文件路径。
	refsDir := filepath.Join(dir, "references")
	if entries, err := os.ReadDir(refsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				skill.References = append(skill.References, filepath.Join(refsDir, e.Name()))
			}
		}
	}

	return skill, nil
}

// parseFrontmatter extracts YAML-like frontmatter (key: value pairs between --- delimiters).
// parseFrontmatter 解析类 YAML 的 frontmatter：内容需以 "---" 开头，到下一行只含 "---" 为止，
// 中间每行按第一个冒号切成 key: value。刻意不支持嵌套与列表——技能元信息只需要扁平键值对。
func parseFrontmatter(content string) (map[string]string, string) {
	fm := make(map[string]string)

	// 不以 --- 开头即视为没有 frontmatter：返回空 map，整个内容都算正文。
	if !strings.HasPrefix(content, "---") {
		return fm, content
	}

	// 按行切分后，从第 1 行起找收尾的 "---"（第 0 行是开头的分隔线）。
	lines := strings.SplitN(content, "\n", -1)
	endIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			endIdx = i
			break
		}
	}
	// 找不到收尾分隔线：frontmatter 不完整，放弃解析，原文全部当正文返回。
	if endIdx < 0 {
		return fm, content
	}

	// 解析两条分隔线之间的每一行：按第一个冒号切 key/value（idx > 0 要求冒号不在行首）。
	for _, line := range lines[1:endIdx] {
		if idx := strings.Index(line, ":"); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])
			fm[key] = val
		}
	}

	// 收尾分隔线之后的所有行重新拼回，即为正文。
	body := strings.Join(lines[endIdx+1:], "\n")
	return fm, body
}

// parseProfileSections splits content by ## headers into a map.
// parseProfileSections 把 PROFILES.md 按 "## " 二级标题切成多个分节：
// 标题文本作为 Profile 名，标题之下直到下一个标题的内容作为该 Profile 的 SystemAddition；
// 首个 ## 之前的内容没有归属，会被直接丢弃。
func parseProfileSections(content string) map[string]ProfileEntry {
	profiles := make(map[string]ProfileEntry)
	scanner := bufio.NewScanner(strings.NewReader(content)) // 逐行扫描器
	var currentName string                                  // 当前正在收集的 Profile 名；空表示尚未遇到任何标题
	var currentLines []string                               // 当前分节已累积的内容行

	// flush 把累积中的分节写入结果 map：内容行拼接并去掉首尾空白。
	flush := func() {
		if currentName != "" {
			profiles[currentName] = ProfileEntry{
				SystemAddition: strings.TrimSpace(strings.Join(currentLines, "\n")),
			}
		}
	}

	// 主扫描循环：遇到新标题先结算上一节再开新节，普通行归入当前节。
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "## ") {
			flush()                                                          // 结算上一个分节
			currentName = strings.TrimSpace(strings.TrimPrefix(line, "## ")) // 剥掉 "## " 前缀得到 Profile 名
			currentLines = nil                                               // 重置内容缓冲
		} else if currentName != "" {
			currentLines = append(currentLines, line)
		}
	}
	flush() // 文件扫描结束后结算最后一个分节

	return profiles
}

// readFileIfExists reads a file, returning "" if it doesn't exist.
// readFileIfExists 是"可选文件"的读取协议：文件不存在返回空串且不报错（缺文件是合法状态），
// 其他 IO 错误（权限、磁盘故障等）如实上抛。
func readFileIfExists(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// 区分"不存在"与"真错误"：前者静默降级为空串。
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}
