package stdlib

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jinyitao123/loom"
)

// SkillMatchResult represents a skill matched to the user's message.
type SkillMatchResult struct {
	Skill SkillDef
	Score float64
}

// SkillMatcher determines which skills are relevant for a given user message.
type SkillMatcher interface {
	Match(userMessage string, skills []SkillDef) []SkillMatchResult
}

// KeywordMatcher is a simple keyword-based skill matcher.
// It matches skills whose Name or Description words overlap with the user's message.
// Words ≤ 3 characters are ignored to avoid noise.
type KeywordMatcher struct{}

func (m *KeywordMatcher) Match(userMessage string, skills []SkillDef) []SkillMatchResult {
	msgWords := extractKeywords(userMessage)
	if len(msgWords) == 0 {
		return nil
	}

	var results []SkillMatchResult
	for _, skill := range skills {
		skillWords := extractKeywords(skill.Name + " " + skill.Description)
		score := overlapScore(msgWords, skillWords)
		if score > 0 {
			results = append(results, SkillMatchResult{Skill: skill, Score: score})
		}
	}

	// Sort by score descending.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	return results
}

// extractKeywords splits text into lowercase words > 3 chars.
func extractKeywords(text string) map[string]bool {
	words := make(map[string]bool)
	for _, w := range strings.Fields(strings.ToLower(text)) {
		w = strings.Trim(w, ".,!?;:\"'()[]{}") // strip punctuation
		if len(w) > 3 {
			words[w] = true
		}
	}
	return words
}

// overlapScore counts the fraction of skill keywords found in the message.
func overlapScore(msgWords, skillWords map[string]bool) float64 {
	if len(skillWords) == 0 {
		return 0
	}
	matches := 0
	for w := range skillWords {
		if msgWords[w] {
			matches++
		}
	}
	return float64(matches) / float64(len(skillWords))
}

// PromptConfig configures the PromptAssembleStep.
type PromptConfig struct {
	Identity              IdentitySpec
	Skills                []SkillDef
	Profile               string            // active profile name
	ProfilesMap           map[string]string  // profile name → content
	MaxSystemPromptTokens int               // token budget for system prompt
	MaxSkillBodyTokens    int               // max tokens per skill body (0 = unlimited)
	SkillMatcher          SkillMatcher
}

// NewPromptAssembleStep creates a step that assembles the system prompt dynamically.
//
// Tiered assembly:
//  1. Core identity (always)
//  2. Skill index — all skills listed by name+description (always)
//  3. Extended identity (if budget allows)
//  4. Profile overlay (if active)
//  5. Matched skill bodies (max 2 per turn, if budget allows)
func NewPromptAssembleStep(cfg PromptConfig) loom.Step {
	return func(_ context.Context, state loom.State) (loom.State, error) {
		var parts []string
		budget := cfg.MaxSystemPromptTokens
		if budget <= 0 {
			budget = 8000
		}

		// 1. Core identity (always included).
		parts = append(parts, cfg.Identity.Core)
		used := estimateTokens(cfg.Identity.Core)

		// 2. Skill index — always list all skills by name + description.
		if len(cfg.Skills) > 0 {
			var index []string
			index = append(index, "\n## Available Skills")
			for _, s := range cfg.Skills {
				index = append(index, fmt.Sprintf("- **%s**: %s", s.Name, s.Description))
			}
			indexStr := strings.Join(index, "\n")
			parts = append(parts, indexStr)
			used += estimateTokens(indexStr)
		}

		// 3. Extended identity (if budget allows).
		if cfg.Identity.Extended != "" {
			extTokens := estimateTokens(cfg.Identity.Extended)
			if used+extTokens <= budget {
				parts = append(parts, cfg.Identity.Extended)
				used += extTokens
			}
		}

		// 4. Profile overlay.
		if cfg.Profile != "" {
			if profileContent, ok := cfg.ProfilesMap[cfg.Profile]; ok {
				profileTokens := estimateTokens(profileContent)
				if used+profileTokens <= budget {
					parts = append(parts, "\n## Profile: "+cfg.Profile+"\n"+profileContent)
					used += profileTokens
				}
			}
		}

		// 5. Matched skill bodies (max 2 per turn).
		var activeSkills []string
		if cfg.SkillMatcher != nil {
			userMsg := GetString(state, "last_user_message", "")
			if userMsg != "" {
				matches := cfg.SkillMatcher.Match(userMsg, cfg.Skills)
				loaded := 0
				for _, m := range matches {
					if loaded >= 2 {
						break
					}
					body := m.Skill.Body
					if body == "" {
						continue
					}

					// Truncate if body too large.
					bodyTokens := estimateTokens(body)
					if cfg.MaxSkillBodyTokens > 0 && bodyTokens > cfg.MaxSkillBodyTokens {
						// Rough truncation.
						maxChars := cfg.MaxSkillBodyTokens * 4
						if maxChars < len(body) {
							body = body[:maxChars] + "\n\n[truncated]"
							bodyTokens = cfg.MaxSkillBodyTokens
						}
					}

					if used+bodyTokens <= budget {
						parts = append(parts, fmt.Sprintf("\n## Skill: %s\n%s", m.Skill.Name, body))
						used += bodyTokens
						activeSkills = append(activeSkills, m.Skill.Name)
						loaded++
					}
				}
			}
		}

		prompt := strings.Join(parts, "\n\n")
		return loom.State{
			"__system_prompt": prompt,
			"__active_skills": activeSkills,
		}, nil
	}
}

// estimateTokens gives a rough token count (~4 chars per token).
func estimateTokens(s string) int {
	return len(s)/4 + 1
}
