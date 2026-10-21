package interactive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Skill is a set of instructions in a SKILL.md file. The model sees its name,
// description and location in the system prompt and reads the file when a
// task matches; a prompt "/skill:<name> [args]" inlines it explicitly.
// Skill 是 SKILL.md 中的一组说明：模型在系统提示词中看到名称、说明与位置，任务匹配时再读取文件；
// 提问 "/skill:<name> [参数]" 会把它直接展开进提问。
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
	Location    string `json:"location"`
}

// maxSkillFile bounds a SKILL.md file.
const maxSkillFile = 256 << 10

// LoadSkills reads skills from each root: a SKILL.md file, a directory that
// holds SKILL.md, or a directory of such directories. location labels where
// they came from ("user", "project" or "path"). Unreadable or malformed skills
// are skipped; the first skill with a given name wins.
// LoadSkills 从每个根读取技能：SKILL.md 文件、含 SKILL.md 的目录或其上一级目录；
// 无法读取或格式不对的跳过，同名以先读到的为准。
func LoadSkills(location string, roots ...string) []Skill {
	var skills []Skill
	seen := map[string]bool{}
	add := func(path string) {
		s, err := readSkill(path)
		if err != nil || seen[s.Name] {
			return
		}
		s.Location = location
		seen[s.Name] = true
		skills = append(skills, s)
	}
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			add(root)
			continue
		}
		if _, err := os.Stat(filepath.Join(root, "SKILL.md")); err == nil {
			add(filepath.Join(root, "SKILL.md"))
			continue
		}
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if e.IsDir() {
				add(filepath.Join(root, e.Name(), "SKILL.md"))
			}
		}
	}
	return skills
}

func readSkill(path string) (Skill, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Skill{}, err
	}
	if info.Size() > maxSkillFile {
		return Skill{}, errors.New("skill file too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, err
	}
	front, _ := splitFrontMatter(string(data))
	s := Skill{Name: front["name"], Description: front["description"], Path: path}
	if s.Name == "" {
		s.Name = filepath.Base(filepath.Dir(path))
	}
	if s.Description == "" {
		return Skill{}, errors.New("skill has no description")
	}
	return s, nil
}

// splitFrontMatter parses a leading "---" block of "key: value" lines.
func splitFrontMatter(text string) (map[string]string, string) {
	fields := map[string]string{}
	text = strings.TrimPrefix(text, "\uFEFF")
	if !strings.HasPrefix(text, "---") {
		return fields, text
	}
	rest := strings.TrimLeft(text[3:], "\r\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fields, text
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		fields[strings.TrimSpace(key)] = value
	}
	body := rest[end+4:]
	return fields, strings.TrimLeft(body, "\r\n")
}

// SkillsPrompt is the system prompt section listing the skills.
func SkillsPrompt(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	sorted := append([]Skill(nil), skills...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var b strings.Builder
	b.WriteString("\n\nThe following skills provide specialized instructions for specific tasks.\n")
	b.WriteString("Use the read tool to load a skill's file when the task matches its description.\n")
	b.WriteString("When a skill file references a relative path, resolve it against the skill directory.\n\n")
	b.WriteString("<available_skills>\n")
	for _, s := range sorted {
		fmt.Fprintf(&b, "  <skill>\n    <name>%s</name>\n    <description>%s</description>\n    <location>%s</location>\n  </skill>\n", s.Name, s.Description, s.Path)
	}
	b.WriteString("</available_skills>")
	return b.String()
}

// expandSkillCommand replaces "/skill:<name> [args]" with the skill body
// followed by the arguments. Other text is returned unchanged.
func expandSkillCommand(text string, skills []Skill) (string, error) {
	if !strings.HasPrefix(text, "/skill:") {
		return text, nil
	}
	name, args, _ := strings.Cut(strings.TrimPrefix(text, "/skill:"), " ")
	name = strings.TrimSpace(name)
	for _, s := range skills {
		if s.Name != name {
			continue
		}
		data, err := os.ReadFile(s.Path)
		if err != nil {
			return "", fmt.Errorf("skill %s: %w", name, err)
		}
		_, body := splitFrontMatter(string(data))
		expanded := fmt.Sprintf("<skill name=%q location=%q>\nReferences are relative to %s.\n\n%s\n</skill>", s.Name, s.Path, filepath.Dir(s.Path), strings.TrimSpace(body))
		if args = strings.TrimSpace(args); args != "" {
			expanded += "\n\n" + args
		}
		return expanded, nil
	}
	return "", fmt.Errorf("unknown skill %q", name)
}
