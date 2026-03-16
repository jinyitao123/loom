package stdlib

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// IdentitySpec holds tiered identity content (v1.3).
// Core is always loaded; Extended is loaded when budget allows.
type IdentitySpec struct {
	Core     string `json:"core,omitempty"`     // text before first ## heading — always in prompt
	Extended string `json:"extended,omitempty"` // text after first ## heading — loaded when budget allows
	Raw      string `json:"raw,omitempty"`      // full original content
}

// AgentSpec is the unified agent specification loaded from files or constructed programmatically.
type AgentSpec struct {
	SystemPrompt string            `json:"system_prompt,omitempty"` // v1.2 compat — equals Identity.Raw
	Identity     IdentitySpec      `json:"identity,omitempty"`      // v1.3: tiered identity
	Profiles     map[string]string `json:"profiles,omitempty"`
	Skills       []SkillDef        `json:"skills,omitempty"`
	HealthCheck  *HealthCheckDef   `json:"health_check,omitempty"`
}

// SkillDef represents a skill loaded from a SKILL.md file.
type SkillDef struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Body        string   `json:"body,omitempty"`
	Scripts     []string `json:"scripts,omitempty"`
	References  []string `json:"references,omitempty"`
}

// HealthCheckDef holds heartbeat/health-check configuration.
type HealthCheckDef struct {
	Content string `json:"content,omitempty"`
}

// splitTiered splits content at the first ## heading into Core (before) and Extended (after).
func splitTiered(content string) (core, extended string) {
	idx := strings.Index(content, "\n## ")
	if idx < 0 {
		// Also check if it starts with ##
		if strings.HasPrefix(content, "## ") {
			return "", content
		}
		return strings.TrimSpace(content), ""
	}
	return strings.TrimSpace(content[:idx]), strings.TrimSpace(content[idx+1:])
}

// LoadFromDirectory loads an AgentSpec from a directory using priority-based file detection.
//
// Priority:
//  1. CLAUDE.md — if present, becomes Identity (overrides SOUL+IDENTITY)
//  2. AGENTS.md — same as CLAUDE.md (OpenAI convention)
//  3. SOUL.md + IDENTITY.md — SOUL → Core, IDENTITY → Extended
func LoadFromDirectory(dir string) (*AgentSpec, error) {
	spec := &AgentSpec{
		Profiles: make(map[string]string),
	}

	// Priority 1: CLAUDE.md overrides everything.
	if content, err := readFileIfExists(filepath.Join(dir, "CLAUDE.md")); err != nil {
		return nil, err
	} else if content != "" {
		core, extended := splitTiered(content)
		spec.Identity = IdentitySpec{Core: core, Extended: extended, Raw: content}
		spec.SystemPrompt = content
	} else if content, err := readFileIfExists(filepath.Join(dir, "AGENTS.md")); err != nil {
		return nil, err
	} else if content != "" {
		core, extended := splitTiered(content)
		spec.Identity = IdentitySpec{Core: core, Extended: extended, Raw: content}
		spec.SystemPrompt = content
	} else {
		// Priority 3: SOUL.md + IDENTITY.md
		var rawParts []string
		soul, err := readFileIfExists(filepath.Join(dir, "SOUL.md"))
		if err != nil {
			return nil, err
		}
		identity, err := readFileIfExists(filepath.Join(dir, "IDENTITY.md"))
		if err != nil {
			return nil, err
		}

		// SOUL.md: split into Core/Extended at first ##
		soulCore, soulExtended := splitTiered(soul)
		spec.Identity.Core = soulCore

		// Combine: SOUL's extended parts + IDENTITY.md → Extended
		var extParts []string
		if soulExtended != "" {
			extParts = append(extParts, soulExtended)
		}
		if identity != "" {
			extParts = append(extParts, strings.TrimSpace(identity))
		}
		spec.Identity.Extended = strings.Join(extParts, "\n\n")

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
	if content, err := readFileIfExists(filepath.Join(dir, "PROFILES.md")); err != nil {
		return nil, err
	} else if content != "" {
		spec.Profiles = parseProfileSections(content)
	}

	// HEARTBEAT.md
	if content, err := readFileIfExists(filepath.Join(dir, "HEARTBEAT.md")); err != nil {
		return nil, err
	} else if content != "" {
		spec.HealthCheck = &HealthCheckDef{Content: content}
	}

	// skills/ subdirectories.
	skillsDir := filepath.Join(dir, "skills")
	if entries, err := os.ReadDir(skillsDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			skill, err := loadSkill(filepath.Join(skillsDir, entry.Name()))
			if err != nil {
				return nil, err
			}
			if skill != nil {
				spec.Skills = append(spec.Skills, *skill)
			}
		}
	}

	return spec, nil
}

// loadSkill loads a SkillDef from a skill directory.
func loadSkill(dir string) (*SkillDef, error) {
	skillFile := filepath.Join(dir, "SKILL.md")
	content, err := readFileIfExists(skillFile)
	if err != nil {
		return nil, err
	}
	if content == "" {
		return nil, nil
	}

	skill := &SkillDef{}
	frontmatter, body := parseFrontmatter(content)
	skill.Body = strings.TrimSpace(body)

	if name, ok := frontmatter["name"]; ok {
		skill.Name = name
	}
	if desc, ok := frontmatter["description"]; ok {
		skill.Description = desc
	}

	// Collect scripts.
	scriptsDir := filepath.Join(dir, "scripts")
	if entries, err := os.ReadDir(scriptsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				skill.Scripts = append(skill.Scripts, filepath.Join(scriptsDir, e.Name()))
			}
		}
	}

	// Collect references.
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
func parseFrontmatter(content string) (map[string]string, string) {
	fm := make(map[string]string)

	if !strings.HasPrefix(content, "---") {
		return fm, content
	}

	lines := strings.SplitN(content, "\n", -1)
	endIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			endIdx = i
			break
		}
	}
	if endIdx < 0 {
		return fm, content
	}

	for _, line := range lines[1:endIdx] {
		if idx := strings.Index(line, ":"); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])
			fm[key] = val
		}
	}

	body := strings.Join(lines[endIdx+1:], "\n")
	return fm, body
}

// parseProfileSections splits content by ## headers into a map.
func parseProfileSections(content string) map[string]string {
	profiles := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(content))
	var currentName string
	var currentLines []string

	flush := func() {
		if currentName != "" {
			profiles[currentName] = strings.TrimSpace(strings.Join(currentLines, "\n"))
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "## ") {
			flush()
			currentName = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			currentLines = nil
		} else if currentName != "" {
			currentLines = append(currentLines, line)
		}
	}
	flush()

	return profiles
}

// readFileIfExists reads a file, returning "" if it doesn't exist.
func readFileIfExists(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}
