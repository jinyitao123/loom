package stdlib_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/stdlib"
)

func TestSpecLoader_MemohSixPiece(t *testing.T) {
	dir := setupMemohFixture(t)

	spec, err := stdlib.LoadFromDirectory(dir)
	assertNoError(t, err)

	// v1.3: tiered identity
	if !strings.Contains(spec.Identity.Core, "You are a test agent") {
		t.Error("SOUL.md Core not loaded")
	}
	if !strings.Contains(spec.Identity.Extended, "Follow instructions") {
		t.Error("IDENTITY.md not loaded into Extended")
	}
	// Raw should contain everything.
	if !strings.Contains(spec.Identity.Raw, "You are a test agent") ||
		!strings.Contains(spec.Identity.Raw, "Follow instructions") {
		t.Error("Identity.Raw incomplete")
	}
	// v1.2 compat
	if !strings.Contains(spec.SystemPrompt, "You are a test agent") {
		t.Error("SystemPrompt compat broken")
	}

	// PROFILES
	if spec.Profiles["engineer"] == "" {
		t.Error("engineer profile not loaded")
	}
	if spec.Profiles["manager"] == "" {
		t.Error("manager profile not loaded")
	}

	// Skills
	if len(spec.Skills) != 2 {
		t.Fatalf("skills count = %d, want 2", len(spec.Skills))
	}
	webSearch := findSkill(spec.Skills, "web-search")
	if webSearch == nil {
		t.Fatal("web-search skill not found")
	}
	if len(webSearch.Scripts) != 1 {
		t.Errorf("scripts count = %d", len(webSearch.Scripts))
	}
	if webSearch.Body == "" {
		t.Error("skill body should be parsed and stored")
	}

	// HEARTBEAT
	if spec.HealthCheck == nil {
		t.Error("HEARTBEAT.md not loaded")
	}
}

func TestSpecLoader_ClaudeMD(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "CLAUDE.md", "You are a Claude-style agent.")

	spec, err := stdlib.LoadFromDirectory(dir)
	assertNoError(t, err)

	if !strings.Contains(spec.Identity.Core, "Claude-style agent") {
		t.Error("CLAUDE.md Core not loaded")
	}
}

func TestSpecLoader_AgentsMD(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "AGENTS.md", "You are an OpenAI-style agent.")

	spec, err := stdlib.LoadFromDirectory(dir)
	assertNoError(t, err)

	if !strings.Contains(spec.Identity.Core, "OpenAI-style agent") {
		t.Error("AGENTS.md Core not loaded")
	}
}

func TestSpecLoader_ClaudeMD_OverridesSoulIdentity(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "SOUL.md", "soul content")
	writeFile(t, dir, "IDENTITY.md", "identity content")
	writeFile(t, dir, "CLAUDE.md", "claude overrides all")

	spec, _ := stdlib.LoadFromDirectory(dir)
	if spec.Identity.Core != "claude overrides all" {
		t.Errorf("CLAUDE.md should override SOUL+IDENTITY Core, got: %s", spec.Identity.Core)
	}
}

func TestSpecLoader_EmptyDirectory(t *testing.T) {
	spec, err := stdlib.LoadFromDirectory(t.TempDir())
	assertNoError(t, err)
	if spec.Identity.Core != "" {
		t.Error("Core should be empty")
	}
	if spec.Identity.Extended != "" {
		t.Error("Extended should be empty")
	}
	if len(spec.Skills) != 0 {
		t.Error("should have no skills")
	}
}

func TestSpecLoader_SkillFrontmatter_Parsing(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skills", "test-skill")
	os.MkdirAll(skillDir, 0755)
	writeFile(t, skillDir, "SKILL.md", `---
name: test-skill
description: A test skill for unit testing
---
## How to use
Follow these steps.
`)

	spec, _ := stdlib.LoadFromDirectory(dir)
	if len(spec.Skills) != 1 {
		t.Fatalf("skills = %d", len(spec.Skills))
	}
	if spec.Skills[0].Name != "test-skill" {
		t.Errorf("name = %s", spec.Skills[0].Name)
	}
	if spec.Skills[0].Description != "A test skill for unit testing" {
		t.Errorf("desc = %s", spec.Skills[0].Description)
	}
	if !strings.Contains(spec.Skills[0].Body, "Follow these steps") {
		t.Error("body not parsed correctly")
	}
}

func TestSpecLoader_TieredSOUL_WithHeadingMarker(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "SOUL.md",
		"You are a concept modeling expert.\n\n"+
			"## Detailed protocol\nPhase 1: Parse objects.\nPhase 2: Build hierarchy.\n")

	spec, _ := stdlib.LoadFromDirectory(dir)

	if spec.Identity.Core != "You are a concept modeling expert." {
		t.Errorf("Core = %q, want text before first ##", spec.Identity.Core)
	}
	if !strings.Contains(spec.Identity.Extended, "Phase 1: Parse objects") {
		t.Error("Extended should contain content after ## heading")
	}
}

func TestSpecLoader_TieredSOUL_NoHeading_AllIsCore(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "SOUL.md", "Simple agent with no headings.")

	spec, _ := stdlib.LoadFromDirectory(dir)

	if spec.Identity.Core != "Simple agent with no headings." {
		t.Errorf("Core = %q, want entire content", spec.Identity.Core)
	}
	if spec.Identity.Extended != "" {
		t.Errorf("Extended should be empty when no headings, got: %q", spec.Identity.Extended)
	}
}

func TestSpecLoader_TieredCLAUDE_SplitAtFirstHeading(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "CLAUDE.md",
		"You are a helpful assistant.\n\n"+
			"## Build commands\npnpm install\n\n"+
			"## Code style\nUse TypeScript.\n")

	spec, _ := stdlib.LoadFromDirectory(dir)

	if spec.Identity.Core != "You are a helpful assistant." {
		t.Errorf("Core = %q", spec.Identity.Core)
	}
	if !strings.Contains(spec.Identity.Extended, "Build commands") {
		t.Error("Extended missing ## Build commands section")
	}
	if !strings.Contains(spec.Identity.Extended, "Code style") {
		t.Error("Extended missing ## Code style section")
	}
}

func TestSpecLoader_SOUL_Plus_IDENTITY_MergedIntoExtended(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "SOUL.md", "Core identity.")
	writeFile(t, dir, "IDENTITY.md", "Detailed reasoning steps.")

	spec, _ := stdlib.LoadFromDirectory(dir)

	if spec.Identity.Core != "Core identity." {
		t.Errorf("Core = %q", spec.Identity.Core)
	}
	if !strings.Contains(spec.Identity.Extended, "Detailed reasoning steps") {
		t.Error("IDENTITY.md should be in Extended")
	}
}

// --- helpers ---

func findSkill(skills []stdlib.SkillDef, name string) *stdlib.SkillDef {
	for i := range skills {
		if skills[i].Name == name {
			return &skills[i]
		}
	}
	return nil
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func setupMemohFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	writeFile(t, dir, "SOUL.md", "You are a test agent.")
	writeFile(t, dir, "IDENTITY.md", "## Protocol\nFollow instructions.")
	writeFile(t, dir, "PROFILES.md", "## engineer\nBe precise.\n## manager\nBe strategic.")
	writeFile(t, dir, "HEARTBEAT.md", "Check every 30m.")

	wsDir := filepath.Join(dir, "skills", "web-search")
	os.MkdirAll(filepath.Join(wsDir, "scripts"), 0755)
	writeFile(t, wsDir, "SKILL.md", "---\nname: web-search\ndescription: Search the web\n---\nSearch instructions.")
	writeFile(t, filepath.Join(wsDir, "scripts"), "search.py", "#!/usr/bin/env python3\nprint('search')")

	ceDir := filepath.Join(dir, "skills", "code-exec")
	os.MkdirAll(ceDir, 0755)
	writeFile(t, ceDir, "SKILL.md", "---\nname: code-exec\ndescription: Execute code\n---\nExecution instructions.")

	return dir
}
