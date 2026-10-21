package interactive

import (
	"os"
	"path/filepath"
	"strings"
)

// MaxContextFile bounds one context file folded into the system prompt;
// larger files are skipped.
const MaxContextFile = 64 << 10

// contextFileNames are tried in order in each directory; the first found is used.
var contextFileNames = []string{"AGENTS.md", "CLAUDE.md"}

// ContextFile is an instructions file found for the working directory.
type ContextFile struct {
	Path    string
	Content string
}

// LoadContextFiles returns the instruction files that apply to cwd: the
// user's <agentDir>/AGENTS.md first, then AGENTS.md (or CLAUDE.md) of every
// directory from the filesystem root down to cwd, so nearer files come later
// and take precedence. Empty, unreadable and oversized files are skipped.
// LoadContextFiles 返回适用于工作目录的说明文件：先是用户级 <agentDir>/AGENTS.md，
// 再按从根目录到工作目录的顺序取各级目录的 AGENTS.md（或 CLAUDE.md），越近越靠后、优先级越高。
func LoadContextFiles(agentDir, cwd string) []ContextFile {
	var files []ContextFile
	seen := map[string]bool{}
	add := func(dir string) {
		for _, name := range contextFileNames {
			path := filepath.Join(dir, name)
			info, err := os.Stat(path)
			if err != nil || info.IsDir() {
				continue
			}
			if info.Size() <= MaxContextFile && !seen[path] {
				if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
					seen[path] = true
					files = append(files, ContextFile{Path: path, Content: string(data)})
				}
			}
			return
		}
	}
	if agentDir != "" {
		if path := filepath.Join(agentDir, "AGENTS.md"); fileExists(path) {
			add(agentDir)
		}
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return files
	}
	var dirs []string
	for dir := abs; ; dir = filepath.Dir(dir) {
		dirs = append(dirs, dir)
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		add(dirs[i])
	}
	return files
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// ContextPrompt renders context files as a system prompt section.
func ContextPrompt(files []ContextFile) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n# Project context\n\nInstructions for this project, from the most general to the most specific:\n")
	for _, f := range files {
		b.WriteString("\n## " + f.Path + "\n\n")
		b.WriteString(strings.TrimRight(f.Content, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}
