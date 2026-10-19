package localtools

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ignoreRule is one parsed line of an ignore file.
type ignoreRule struct {
	segments []string // slash-separated glob segments; "**" spans directories
	negate   bool     // "!pattern" re-includes what an earlier rule excluded
	dirOnly  bool     // "pattern/" matches directories only
}

// ignoreFile holds the rules of one file. Its patterns are relative to the
// file's directory, which is either inside the search root (within, "" for
// the root itself) or above it (above: the search root's path relative to
// that directory).
type ignoreFile struct {
	within string
	above  string
	rules  []ignoreRule
}

// parseIgnore reads gitignore syntax: comments, blank lines, "\#" and "\!"
// escapes, trailing spaces (unless escaped), "!" negation, a trailing "/"
// for directories, and patterns containing "/" anchored to the file's
// directory while others match a name at any depth.
func parseIgnore(data string) []ignoreRule {
	var rules []ignoreRule
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		line = trimTrailingSpaces(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var rule ignoreRule
		switch {
		case strings.HasPrefix(line, "!"):
			rule.negate = true
			line = line[1:]
		case strings.HasPrefix(line, `\!`), strings.HasPrefix(line, `\#`):
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			rule.dirOnly = true
			line = strings.TrimRight(line, "/")
		}
		if line == "" {
			continue
		}
		line = strings.ReplaceAll(line, "[!", "[^")
		if strings.Contains(line, "/") {
			rule.segments = strings.Split(strings.TrimPrefix(line, "/"), "/")
		} else {
			rule.segments = []string{"**", line}
		}
		rules = append(rules, rule)
	}
	return rules
}

// trimTrailingSpaces drops trailing spaces that are not escaped with "\".
func trimTrailingSpaces(line string) string {
	for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
		line = line[:len(line)-1]
	}
	if strings.HasSuffix(line, `\ `) {
		line = line[:len(line)-2] + " "
	}
	return line
}

// match reports whether the rule matches a path relative to its file.
func (r ignoreRule) match(rel string, dir bool) bool {
	if r.dirOnly && !dir {
		return false
	}
	return matchIgnoreSegments(r.segments, strings.Split(rel, "/"))
}

func matchIgnoreSegments(pattern, parts []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			rest := pattern[1:]
			// A trailing "**" matches everything inside, not the directory itself.
			if len(rest) == 0 {
				return len(parts) > 0
			}
			for i := 0; i <= len(parts); i++ {
				if matchIgnoreSegments(rest, parts[i:]) {
					return true
				}
			}
			return false
		}
		if len(parts) == 0 {
			return false
		}
		if ok, _ := path.Match(pattern[0], parts[0]); !ok {
			return false
		}
		pattern, parts = pattern[1:], parts[1:]
	}
	return len(parts) == 0
}

// ignoreMatcher decides which paths under a search root are ignored, the way
// git does: global excludes, then .git/info/exclude, then the .gitignore of
// every directory from the repository root down, nearer files and later
// lines winning. A directory's .ignore file applies after its .gitignore.
// .git and node_modules directories are always skipped.
// ignoreMatcher 按 git 的方式判断搜索根下哪些路径被忽略：全局排除、.git/info/exclude、
// 从仓库根往下各级目录的 .gitignore，越近、越靠后的规则越优先；同目录的 .ignore 在 .gitignore 之后生效。
type ignoreMatcher struct {
	files []*ignoreFile // in precedence order, lowest first
}

// ignoreFileNames are read in every directory, in this order.
var ignoreFileNames = []string{".gitignore", ".ignore"}

// newIgnoreMatcher loads the rules that apply above root: global excludes,
// and, inside a git repository, info/exclude and the ignore files of the
// directories between the repository root and root.
func newIgnoreMatcher(root string) *ignoreMatcher {
	m := &ignoreMatcher{}
	repo := gitRoot(root)
	above := ""
	if repo != "" {
		if rel, err := filepath.Rel(repo, root); err == nil && rel != "." {
			above = filepath.ToSlash(rel)
		}
	}
	if global := globalExcludesFile(); global != "" {
		m.add(global, "", above)
	}
	if repo == "" {
		return m
	}
	if info, err := os.Stat(filepath.Join(repo, ".git")); err == nil && info.IsDir() {
		m.add(filepath.Join(repo, ".git", "info", "exclude"), "", above)
	}
	// Directories from the repository root down to root's parent.
	if above != "" {
		parts := strings.Split(above, "/")
		for i := 0; i < len(parts); i++ {
			dir := filepath.Join(append([]string{repo}, parts[:i]...)...)
			rest := strings.Join(parts[i:], "/")
			for _, name := range ignoreFileNames {
				m.add(filepath.Join(dir, name), "", rest)
			}
		}
	}
	return m
}

// enter loads the ignore files of a directory inside the search root (rel
// "" for the root itself).
func (m *ignoreMatcher) enter(root, rel string) {
	for _, name := range ignoreFileNames {
		m.add(filepath.Join(root, filepath.FromSlash(rel), name), rel, "")
	}
}

func (m *ignoreMatcher) add(file, within, above string) {
	data, err := os.ReadFile(file)
	if err != nil {
		return
	}
	if rules := parseIgnore(string(data)); len(rules) > 0 {
		m.files = append(m.files, &ignoreFile{within: within, above: above, rules: rules})
	}
}

// ignored reports whether rel (slash-separated, relative to the search
// root) is ignored.
func (m *ignoreMatcher) ignored(rel string, dir bool) bool {
	if base := path.Base(rel); dir && (base == ".git" || base == "node_modules") {
		return true
	}
	ignored := false
	for _, f := range m.files {
		local := rel
		switch {
		case f.above != "":
			local = f.above + "/" + rel
		case f.within != "":
			if !strings.HasPrefix(rel, f.within+"/") {
				continue
			}
			local = rel[len(f.within)+1:]
		}
		for _, rule := range f.rules {
			if rule.match(local, dir) {
				ignored = !rule.negate
			}
		}
	}
	return ignored
}

// gitRoot returns the nearest directory at or above dir that contains .git,
// or "".
func gitRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// globalExcludesFile is git's core.excludesFile from the user's
// ~/.gitconfig, else $XDG_CONFIG_HOME/git/ignore (~/.config/git/ignore).
func globalExcludesFile() string {
	home, _ := os.UserHomeDir()
	if home != "" {
		if data, err := os.ReadFile(filepath.Join(home, ".gitconfig")); err == nil {
			if file := excludesFileSetting(string(data)); file != "" {
				if strings.HasPrefix(file, "~/") {
					file = filepath.Join(home, file[2:])
				}
				return file
			}
		}
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "git", "ignore")
	}
	if home != "" {
		return filepath.Join(home, ".config", "git", "ignore")
	}
	return ""
}

// excludesFileSetting reads core.excludesFile from git config text.
func excludesFileSetting(config string) string {
	inCore := false
	for _, line := range strings.Split(config, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inCore = strings.EqualFold(strings.Trim(line, "[] \t"), "core")
			continue
		}
		if !inCore {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.EqualFold(strings.TrimSpace(key), "excludesfile") {
			return strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return ""
}
