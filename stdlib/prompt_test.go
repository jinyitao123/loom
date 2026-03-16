package stdlib_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/stdlib"
)

func TestPromptAssemble_TierZero_AlwaysLoaded(t *testing.T) {
	cfg := stdlib.PromptConfig{
		Identity: stdlib.IdentitySpec{
			Core:     "You are a test agent.",
			Extended: "## Detailed protocol\nPhase 1...",
		},
		Skills: []stdlib.SkillDef{
			{Name: "search", Description: "Search the web for information"},
			{Name: "code", Description: "Execute code snippets"},
		},
		MaxSystemPromptTokens: 4000,
		SkillMatcher:          &stdlib.KeywordMatcher{},
	}

	step := stdlib.NewPromptAssembleStep(cfg)
	result, err := step(context.Background(), loom.State{"last_user_message": "hello"})
	assertNoError(t, err)

	prompt := result["__system_prompt"].(string)

	if !strings.Contains(prompt, "You are a test agent") {
		t.Error("Core identity missing from prompt")
	}
	if !strings.Contains(prompt, "**search**") || !strings.Contains(prompt, "**code**") {
		t.Error("Skill index incomplete")
	}
}

func TestPromptAssemble_SkillBodyLoadedOnMatch(t *testing.T) {
	cfg := stdlib.PromptConfig{
		Identity: stdlib.IdentitySpec{Core: "You are a test agent."},
		Skills: []stdlib.SkillDef{
			{Name: "search", Description: "Search the web for information", Body: "## Search Instructions\nUse Google."},
			{Name: "code", Description: "Execute code snippets", Body: "## Code Instructions\nUse Python."},
		},
		MaxSystemPromptTokens: 8000,
		SkillMatcher:          &stdlib.KeywordMatcher{},
	}

	step := stdlib.NewPromptAssembleStep(cfg)
	result, _ := step(context.Background(), loom.State{"last_user_message": "search for climate data"})

	prompt := result["__system_prompt"].(string)
	active := result["__active_skills"].([]string)

	if !strings.Contains(prompt, "Search Instructions") {
		t.Error("Matched skill body not loaded")
	}
	if strings.Contains(prompt, "Code Instructions") {
		t.Error("Non-matched skill body should not be loaded")
	}
	if len(active) != 1 || active[0] != "search" {
		t.Errorf("active skills = %v, want [search]", active)
	}
}

func TestPromptAssemble_NoSkillMatchedOnIrrelevantMessage(t *testing.T) {
	cfg := stdlib.PromptConfig{
		Identity: stdlib.IdentitySpec{Core: "Agent."},
		Skills: []stdlib.SkillDef{
			{Name: "deploy", Description: "Deploy applications to production", Body: "Long deploy instructions..."},
		},
		MaxSystemPromptTokens: 4000,
		SkillMatcher:          &stdlib.KeywordMatcher{},
	}

	step := stdlib.NewPromptAssembleStep(cfg)
	result, _ := step(context.Background(), loom.State{"last_user_message": "what is the weather?"})

	prompt := result["__system_prompt"].(string)
	active := result["__active_skills"].([]string)

	if strings.Contains(prompt, "Long deploy instructions") {
		t.Error("Irrelevant skill body should not be loaded")
	}
	if len(active) != 0 {
		t.Errorf("no skills should match, got %v", active)
	}
}

func TestPromptAssemble_ExtendedDroppedWhenBudgetTight(t *testing.T) {
	cfg := stdlib.PromptConfig{
		Identity: stdlib.IdentitySpec{
			Core:     strings.Repeat("core ", 200),
			Extended: strings.Repeat("extended ", 2000),
		},
		MaxSystemPromptTokens: 500,
		SkillMatcher:          &stdlib.KeywordMatcher{},
	}

	step := stdlib.NewPromptAssembleStep(cfg)
	result, _ := step(context.Background(), loom.State{"last_user_message": "hello"})

	prompt := result["__system_prompt"].(string)
	if !strings.Contains(prompt, "core") {
		t.Error("Core must always be present")
	}
	if strings.Contains(prompt, "extended") {
		t.Error("Extended should be dropped when budget is tight")
	}
}

func TestPromptAssemble_ProfileOverlay(t *testing.T) {
	cfg := stdlib.PromptConfig{
		Identity:              stdlib.IdentitySpec{Core: "Agent."},
		Profile:               "engineer",
		ProfilesMap:           map[string]string{"engineer": "Be precise and technical.", "manager": "Be strategic."},
		MaxSystemPromptTokens: 4000,
		SkillMatcher:          &stdlib.KeywordMatcher{},
	}

	step := stdlib.NewPromptAssembleStep(cfg)
	result, _ := step(context.Background(), loom.State{"last_user_message": "hello"})

	prompt := result["__system_prompt"].(string)
	if !strings.Contains(prompt, "Be precise and technical") {
		t.Error("Engineer profile overlay missing")
	}
	if strings.Contains(prompt, "Be strategic") {
		t.Error("Manager profile should not be loaded")
	}
}

func TestPromptAssemble_MaxTwoSkillsPerTurn(t *testing.T) {
	skills := make([]stdlib.SkillDef, 5)
	for i := range skills {
		skills[i] = stdlib.SkillDef{
			Name:        fmt.Sprintf("skill-%d", i),
			Description: "keyword match here",
			Body:        fmt.Sprintf("Body of skill %d", i),
		}
	}

	cfg := stdlib.PromptConfig{
		Identity:              stdlib.IdentitySpec{Core: "Agent."},
		Skills:                skills,
		MaxSystemPromptTokens: 50000,
		SkillMatcher:          &stdlib.KeywordMatcher{},
	}

	step := stdlib.NewPromptAssembleStep(cfg)
	result, _ := step(context.Background(), loom.State{"last_user_message": "keyword match here"})

	active := result["__active_skills"].([]string)
	if len(active) > 2 {
		t.Errorf("max 2 skills per turn, got %d: %v", len(active), active)
	}
}

// --- SkillMatcher tests ---

func TestKeywordMatcher_MatchFound(t *testing.T) {
	m := &stdlib.KeywordMatcher{}
	skills := []stdlib.SkillDef{
		{Name: "search", Description: "Search the web for information"},
		{Name: "deploy", Description: "Deploy applications to production"},
	}

	results := m.Match("please search for climate data", skills)
	if len(results) == 0 {
		t.Fatal("expected at least one match")
	}
	if results[0].Skill.Name != "search" {
		t.Errorf("best match = %s, want search", results[0].Skill.Name)
	}
}

func TestKeywordMatcher_NoMatch(t *testing.T) {
	m := &stdlib.KeywordMatcher{}
	skills := []stdlib.SkillDef{
		{Name: "deploy", Description: "Deploy applications to production"},
	}

	results := m.Match("what is the weather today", skills)
	if len(results) != 0 {
		t.Errorf("expected no match, got %d", len(results))
	}
}

func TestKeywordMatcher_SortedByScore(t *testing.T) {
	m := &stdlib.KeywordMatcher{}
	skills := []stdlib.SkillDef{
		{Name: "low", Description: "this skill handles database operations"},
		{Name: "high", Description: "search web information data retrieval"},
	}

	results := m.Match("search for information and data", skills)
	if len(results) < 2 {
		t.Skipf("need 2 matches, got %d", len(results))
	}
	if results[0].Score <= results[1].Score {
		t.Error("results should be sorted by score descending")
	}
}

func TestKeywordMatcher_ShortWordsIgnored(t *testing.T) {
	m := &stdlib.KeywordMatcher{}
	skills := []stdlib.SkillDef{
		{Name: "bad", Description: "do it on the go as if by me"},
	}

	results := m.Match("do it on the go", skills)
	if len(results) != 0 {
		t.Errorf("short words should be ignored, got %d matches", len(results))
	}
}
