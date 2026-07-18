package stdlib

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jinyitao123/loom"
)

// SkillMatchResult represents a skill matched to the user's message.
// SkillMatchResult 表示与用户消息命中的一条技能及其相关性得分，供上层按分数排序取舍。
type SkillMatchResult struct {
	Skill SkillDef // 命中的技能定义（名称、描述、正文等）
	Score float64  // 相关性得分（0~1），越大越相关
}

// SkillMatcher determines which skills are relevant for a given user message.
// SkillMatcher 是技能匹配器抽象：输入用户消息与全部技能，输出按相关性降序的命中列表。
// 做成接口是为了允许上层替换成向量检索等更强的实现；KeywordMatcher 只是零依赖的默认实现。
type SkillMatcher interface {
	// Match 返回按 Score 降序排列的命中结果；返回空表示没有任何技能与消息相关。
	Match(userMessage string, skills []SkillDef) []SkillMatchResult
}

// KeywordMatcher is a simple keyword-based skill matcher.
// It matches skills whose Name or Description words overlap with the user's message.
// Words ≤ 3 characters are ignored to avoid noise.
// KeywordMatcher 是纯关键词匹配器：把用户消息与技能"名称+描述"各自切成关键词集合，
// 用重叠率打分。不做语义理解，胜在无需外部服务、离线零成本即可工作。
type KeywordMatcher struct{}

func (m *KeywordMatcher) Match(userMessage string, skills []SkillDef) []SkillMatchResult {
	// 先把用户消息切成关键词集合（英文取长词、CJK 取单字+二元组，见 extractKeywords）。
	msgWords := extractKeywords(userMessage)
	// 消息里提取不出任何关键词（全是短词或标点）时无从匹配，直接返回空。
	if len(msgWords) == 0 {
		return nil
	}

	var results []SkillMatchResult
	// 逐个技能计算与消息的关键词重叠率。
	for _, skill := range skills {
		// 技能侧的关键词只取"名称 + 描述"；正文不参与匹配——正文可能很长且噪声大。
		skillWords := extractKeywords(skill.Name + " " + skill.Description)
		// 得分 = 技能关键词被消息覆盖的比例（见 overlapScore）。
		score := overlapScore(msgWords, skillWords)
		// 只保留得分为正的技能，完全不相关的直接丢弃。
		if score > 0 {
			results = append(results, SkillMatchResult{Skill: skill, Score: score})
		}
	}

	// Sort by score descending.
	// 按得分降序排序：调用方（提示词组装）只会取前几名注入正文，顺序即优先级。
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	// 返回全部命中结果；截断策略（如每轮最多 2 条）由调用方决定，这里不越权。
	return results
}

// extractKeywords splits text into keywords.
// English: space-separated words > 3 chars.
// CJK: individual characters and 2-char bigrams (since CJK has no spaces).
// extractKeywords \u628a\u6587\u672c\u5207\u6210\u5173\u952e\u8bcd\u96c6\u5408\uff08map \u5f53 set \u7528\uff0c\u5929\u7136\u53bb\u91cd\uff09\u3002
// \u82f1\u6587\u6309\u7a7a\u683c\u5206\u8bcd\u5e76\u53ea\u4fdd\u7559\u957f\u5ea6 > 3 \u7684\u8bcd\u4ee5\u8fc7\u6ee4\u566a\u58f0\uff1b\u4e2d\u65e5\u97e9\u6587\u5b57\u6ca1\u6709\u7a7a\u683c\u5206\u8bcd\uff0c
// \u56e0\u6b64\u9000\u5316\u4e3a"\u5355\u5b57 + \u76f8\u90bb\u4e8c\u5143\u7ec4"\u7684\u7ec4\u5408\u5207\u5206\u2014\u2014\u5355\u5b57\u4fdd\u53ec\u56de\uff0c\u4e8c\u5143\u7ec4\u4fdd\u7cbe\u5ea6\u3002
func extractKeywords(text string) map[string]bool {
	words := make(map[string]bool) // \u5173\u952e\u8bcd\u96c6\u5408\uff1akey \u662f\u8bcd\uff0cvalue \u6052\u4e3a true
	lower := strings.ToLower(text) // \u7edf\u4e00\u8f6c\u5c0f\u5199\uff0c\u4f7f\u5339\u914d\u5927\u5c0f\u5199\u4e0d\u654f\u611f

	// English words (space-separated, > 3 chars)
	// \u82f1\u6587\u8def\u5f84\uff1a\u6309\u7a7a\u767d\u5207\u8bcd\uff0c\u5265\u6389\u4e24\u7aef\u6807\u70b9\u540e\u53ea\u6536\u957f\u5ea6 > 3 \u7684\u8bcd\uff08"the"\u3001"is" \u8fd9\u7c7b\u77ed\u8bcd\u5168\u88ab\u8fc7\u6ee4\uff09\u3002
	for _, w := range strings.Fields(lower) {
		// Trim \u7684\u5b57\u7b26\u96c6\u540c\u65f6\u8986\u76d6 ASCII \u6807\u70b9\u4e0e\u5168\u89d2\u4e2d\u6587\u6807\u70b9\uff08\u9017\u53f7\u3001\u53e5\u53f7\u3001\u5f15\u53f7\u3001\u62ec\u53f7\u7b49\uff09\uff0c
		// \u907f\u514d"\u8bcd+\u6807\u70b9"\u88ab\u5f53\u6210\u4e0d\u540c\u7684\u8bcd\u3002
		w = strings.Trim(w, `.,!?;:"'()[]{}`+"\uff0c\u3002\uff01\uff1f\u3001\uff1b\uff1a\u201c\u201d\u2018\u2019\uff08\uff09\u3010\u3011")
		if len(w) > 3 {
			words[w] = true
		}
	}

	// CJK characters and bigrams
	// CJK \u8def\u5f84\uff1a\u5148\u8f6c\u6210 rune \u5207\u7247\u9010"\u5b57"\u626b\u63cf\uff0c\u907f\u514d\u6309\u5b57\u8282\u904d\u5386\u8bef\u5207\u591a\u5b57\u8282\u5b57\u7b26\u3002
	runes := []rune(lower)
	for i, r := range runes {
		if isCJK(r) {
			// \u5355\u5b57\u5165\u96c6\uff1a\u4fdd\u8bc1\u6700\u4f4e\u9650\u5ea6\u7684\u53ec\u56de\u7387\u3002
			words[string(r)] = true // single char
			// \u76f8\u90bb\u4e24\u4e2a\u5b57\u7b26\u90fd\u662f CJK \u65f6\u518d\u6536\u4e00\u4e2a\u4e8c\u5143\u7ec4\uff0c\u5145\u5f53\u6700\u7c97\u7cd9\u7684"\u5206\u8bcd"\uff0c\u63d0\u5347\u5339\u914d\u7cbe\u5ea6\u3002
			if i+1 < len(runes) && isCJK(runes[i+1]) {
				words[string(runes[i:i+2])] = true // bigram
			}
		}
	}

	return words
}

// isCJK \u5224\u65ad\u5355\u4e2a rune \u662f\u5426\u5c5e\u4e8e\u4e2d\u65e5\u97e9\u6587\u5b57\u3002
// \u56db\u4e2a\u533a\u95f4\u4f9d\u6b21\u4e3a\uff1a\u4e2d\u6587\u7edf\u4e00\u8868\u610f\u6587\u5b57\uff08\u6c49\u5b57\u4e3b\u533a\uff09\u3001\u65e5\u6587\u5e73\u5047\u540d\u3001\u65e5\u6587\u7247\u5047\u540d\u3001\u97e9\u6587\u97f3\u8282\uff1b
// \u547d\u4e2d\u4efb\u4e00\u533a\u95f4\u5373\u8d70"\u5355\u5b57+\u4e8c\u5143\u7ec4"\u5207\u5206\u8def\u5f84\u3002
func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || // CJK Unified Ideographs
		(r >= 0x3040 && r <= 0x309F) || // Hiragana
		(r >= 0x30A0 && r <= 0x30FF) || // Katakana
		(r >= 0xAC00 && r <= 0xD7AF) // Hangul
}

// overlapScore counts the fraction of skill keywords found in the message.
// overlapScore \u8ba1\u7b97\u91cd\u53e0\u7387\u5f97\u5206\uff1a\u6280\u80fd\u5173\u952e\u8bcd\u4e2d\u6709\u591a\u5927\u6bd4\u4f8b\u51fa\u73b0\u5728\u7528\u6237\u6d88\u606f\u91cc\u3002
// \u523b\u610f\u4ee5\u6280\u80fd\u4fa7\u8bcd\u6570\u4e3a\u5206\u6bcd\uff08\u800c\u975e\u6d88\u606f\u4fa7\uff09\uff0c\u8fd9\u6837\u63cf\u8ff0\u7cbe\u70bc\u7684\u6280\u80fd\u4e0d\u4f1a\u56e0\u4e3a\u6d88\u606f\u5f88\u957f\u800c\u88ab\u7a00\u91ca\u3002
func overlapScore(msgWords, skillWords map[string]bool) float64 {
	// \u6280\u80fd\u6ca1\u6709\u4efb\u4f55\u5173\u952e\u8bcd\u65f6\u65e0\u4ece\u5339\u914d\uff0c\u5224 0 \u5206\uff0c\u540c\u65f6\u907f\u514d\u4e0b\u9762\u9664\u96f6\u3002
	if len(skillWords) == 0 {
		return 0
	}
	matches := 0
	// \u7edf\u8ba1\u6280\u80fd\u5173\u952e\u8bcd\u88ab\u6d88\u606f\u547d\u4e2d\u7684\u4e2a\u6570\u3002
	for w := range skillWords {
		if msgWords[w] {
			matches++
		}
	}
	// \u5f52\u4e00\u5316\u5230 0~1\uff1a\u547d\u4e2d\u6570 / \u6280\u80fd\u5173\u952e\u8bcd\u603b\u6570\u3002
	return float64(matches) / float64(len(skillWords))
}

// PromptConfig configures the PromptAssembleStep.
// PromptConfig 是提示词组装步骤的配置：描述"身份 + 技能 + Profile + 运行时上下文"
// 各层素材的来源，以及分层装入时的 token 预算。
type PromptConfig struct {
	Identity IdentitySpec // 分层身份：Core 永远注入，Extended 视预算注入
	Skills   []SkillDef   // 全部技能；索引永远注入，正文按匹配得分与预算择优注入
	Profile  string       // active profile name
	// Profile：当前激活的 Profile 名，空串表示不启用覆盖层。
	ProfilesMap map[string]ProfileEntry // profile name → entry
	// ProfilesMap：Profile 名 → 条目的查找表；激活项的 SystemAddition 会叠加进系统提示词。
	Context map[string]any // runtime context (appended as suffix)
	// Context：运行时上下文键值对（如当前时间、会话元数据），渲染成列表作为后缀段追加。
	MaxSystemPromptTokens int // token budget for system prompt
	// MaxSystemPromptTokens：系统提示词的 token 总预算，<=0 时回退默认值 8000。
	MaxSkillBodyTokens int // max tokens per skill body (0 = unlimited)
	// MaxSkillBodyTokens：单条技能正文的 token 上限，超出部分粗暴截断；0 表示不限制。
	SkillMatcher SkillMatcher
	// SkillMatcher：技能匹配器；为 nil 时跳过技能正文层（提示词里只保留技能索引）。
}

// NewPromptAssembleStep creates a step that assembles the system prompt dynamically.
//
// Tiered assembly:
//  1. Core identity (always)
//  2. Skill index — all skills listed by name+description (always)
//  3. Extended identity (if budget allows)
//  4. Profile overlay (if active)
//  5. Matched skill bodies (max 2 per turn, if budget allows)
//
// NewPromptAssembleStep 返回一个"动态组装系统提示词"的步骤（loom.Step）。
// 五层分层装配、逐层记账，在固定 token 预算内按优先级取舍：
//  1. 核心身份 —— 无条件注入，agent 的人格底线永远在场；
//  2. 技能索引 —— 无条件注入，保证模型至少知道自己会什么（哪怕正文没装进来）；
//  3. 扩展身份 —— 预算装得下才注入；
//  4. Profile 覆盖层 —— 有激活的 Profile 且预算装得下才注入；
//  5. 命中的技能正文 —— 按匹配得分从高到低装入，每轮最多 2 条。
//
// 组装结果写入状态键 __system_prompt，下游工具循环步骤会用它覆盖自己的静态配置。
func NewPromptAssembleStep(cfg PromptConfig) loom.Step {
	// 返回的闭包即步骤本体：每轮对话都重新组装一次，因此提示词能随消息与状态动态变化。
	return func(_ context.Context, state loom.State) (loom.State, error) {
		var parts []string // 各层内容按顺序累积，最后统一拼接成完整提示词
		// 确定 token 预算：未配置或非法值时回退到默认 8000。
		budget := cfg.MaxSystemPromptTokens
		if budget <= 0 {
			budget = 8000
		}

		// 1. Core identity (always included).
		// 第 1 层：核心身份无条件写入，并从它开始记账已用 token。
		parts = append(parts, cfg.Identity.Core)
		used := estimateTokens(cfg.Identity.Core)

		// 2. Skill index — always list all skills by name + description.
		// 第 2 层：技能索引同样无条件写入（不做预算门槛检查）——即便正文装不下，
		// 模型也必须知道自己具备哪些能力，才可能在对话中主动引导触发对应技能。
		if len(cfg.Skills) > 0 {
			var index []string
			index = append(index, "\n## Available Skills") // 索引段标题
			// 每个技能一行"名称: 描述"，只暴露元信息，不含正文。
			for _, s := range cfg.Skills {
				index = append(index, fmt.Sprintf("- **%s**: %s", s.Name, s.Description))
			}
			indexStr := strings.Join(index, "\n")
			parts = append(parts, indexStr)
			used += estimateTokens(indexStr) // 索引虽不受门槛限制，但仍要计入已用量
		}

		// 3. Extended identity (if budget allows).
		// 第 3 层：扩展身份属于"锦上添花"，先估算成本，预算装得下才装（装不下就整段放弃）。
		if cfg.Identity.Extended != "" {
			extTokens := estimateTokens(cfg.Identity.Extended)
			if used+extTokens <= budget {
				parts = append(parts, cfg.Identity.Extended)
				used += extTokens
			}
		}

		// 4. Profile overlay.
		// 第 4 层：Profile 覆盖层——按激活的 Profile 名查表，把其 SystemAddition 作为独立小节追加，
		// 让同一个 agent 在不同角色档位下有不同的行为补充；同样受预算约束。
		if cfg.Profile != "" {
			// 查不到条目或补充文本为空都视为无覆盖，静默跳过。
			if entry, ok := cfg.ProfilesMap[cfg.Profile]; ok && entry.SystemAddition != "" {
				profileTokens := estimateTokens(entry.SystemAddition)
				if used+profileTokens <= budget {
					parts = append(parts, "\n## Profile: "+cfg.Profile+"\n"+entry.SystemAddition)
					used += profileTokens
				}
			}
		}

		// 5. Runtime context (appended as suffix).
		// 运行时上下文段：把键值对渲染成列表追加为后缀（注意 map 遍历顺序不保证稳定）。
		if len(cfg.Context) > 0 {
			var ctxLines []string
			ctxLines = append(ctxLines, "\n## 当前上下文") // 上下文段标题
			for k, v := range cfg.Context {
				ctxLines = append(ctxLines, fmt.Sprintf("- %s: %v", k, v))
			}
			ctxStr := strings.Join(ctxLines, "\n")
			ctxTokens := estimateTokens(ctxStr)
			// 整段要么全装、要么全不装，避免上下文列表被截半引起误导。
			if used+ctxTokens <= budget {
				parts = append(parts, ctxStr)
				used += ctxTokens
			}
		}

		// 6. Matched skill bodies (max 2 per turn).
		// 第 5 层（技能正文）：根据本轮用户消息做技能匹配，把得分最高且预算装得下的正文注入。
		// 每轮硬上限 2 条——防止一次性塞入过多技能正文挤占上下文。
		var activeSkills []string // 记录本轮实际注入正文的技能名，写回状态供观测与调试
		if cfg.SkillMatcher != nil {
			// 匹配依据是状态里的 last_user_message；没有用户消息就没有匹配对象，整层跳过。
			userMsg := GetString(state, "last_user_message", "")
			if userMsg != "" {
				// 匹配结果已按得分降序，从高分到低分依次尝试装入。
				matches := cfg.SkillMatcher.Match(userMsg, cfg.Skills)
				loaded := 0 // 本轮已装入的正文条数
				for _, m := range matches {
					// 达到每轮 2 条的硬上限即停止。
					if loaded >= 2 {
						break
					}
					body := m.Skill.Body
					// 正文为空的技能没有可注入的内容，跳过（技能索引里仍然列着它）。
					if body == "" {
						continue
					}

					// Truncate if body too large.
					// 单条正文超过 MaxSkillBodyTokens 时先截断，保证单个技能不会独占预算。
					bodyTokens := estimateTokens(body)
					if cfg.MaxSkillBodyTokens > 0 && bodyTokens > cfg.MaxSkillBodyTokens {
						// Rough truncation.
						// 按 ~4 字符/token 的比例反推最大字符数，按字节截断并附加 [truncated] 标记。
						maxChars := cfg.MaxSkillBodyTokens * 4
						if maxChars < len(body) {
							body = body[:maxChars] + "\n\n[truncated]"
							bodyTokens = cfg.MaxSkillBodyTokens
						}
					}

					// 预算装得下才真正注入；装不下不终止循环，继续尝试后面（可能更短的）候选。
					if used+bodyTokens <= budget {
						parts = append(parts, fmt.Sprintf("\n## Skill: %s\n%s", m.Skill.Name, body))
						used += bodyTokens
						activeSkills = append(activeSkills, m.Skill.Name)
						loaded++
					}
				}
			}
		}

		// 各层之间用空行分隔，拼成最终系统提示词。
		prompt := strings.Join(parts, "\n\n")
		// 只返回增量状态：__system_prompt 供下游工具循环覆盖静态配置，
		// __active_skills 暴露本轮实际激活的技能列表。
		return loom.State{
			"__system_prompt": prompt,
			"__active_skills": activeSkills,
		}, nil
	}
}

// estimateTokens gives a rough token count (~4 chars per token).
// estimateTokens 用"约 4 字符 ≈ 1 token"的经验值粗估 token 数，+1 兜底避免空串估成 0。
// 只求量级正确、够做预算取舍，不追求与真实分词器一致。
func estimateTokens(s string) int {
	return len(s)/4 + 1
}
