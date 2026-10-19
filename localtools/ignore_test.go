package localtools

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// isolateGitConfig keeps the user's global excludes out of a test.
func isolateGitConfig(t *testing.T) string {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func walked(t *testing.T, root string) []string {
	t.Helper()
	var got []string
	if err := walk(context.Background(), root, func(_, rel string) bool { got = append(got, rel); return true }); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	return got
}

func TestParseIgnoreSyntax(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		dir     bool
		want    bool
	}{
		{"*.log", "a/b/x.log", false, true},
		{"/build", "build", true, true},
		{"/build", "src/build", true, false},
		{"docs/*.md", "docs/a.md", false, true},
		{"docs/*.md", "docs/sub/a.md", false, false},
		{"docs/*.md", "x/docs/a.md", false, false},
		{"out/", "out", true, true},
		{"out/", "out", false, false},
		{"**/gen", "a/b/gen", true, true},
		{"logs/**", "logs/a/b.txt", false, true},
		{"logs/**", "logs", true, false},
		{"a/**/z", "a/z", false, true},
		{"a/**/z", "a/b/c/z", false, true},
		{"[!a]bc", "xbc", false, true},
		{"[!a]bc", "abc", false, false},
		{`\#hash`, "#hash", false, true},
		{`\!bang`, "!bang", false, true},
		{"trail   ", "trail", false, true},
		{`space\ `, "space ", false, true},
		{"?.txt", "a.txt", false, true},
	}
	for _, c := range cases {
		rules := parseIgnore(c.pattern)
		if len(rules) != 1 {
			t.Fatalf("%q parsed to %d rules", c.pattern, len(rules))
		}
		if got := rules[0].match(c.path, c.dir); got != c.want {
			t.Errorf("%q vs %q (dir %v) = %v, want %v", c.pattern, c.path, c.dir, got, c.want)
		}
	}
	if rules := parseIgnore("# comment\n\n  \n!keep.log\n"); len(rules) != 1 || !rules[0].negate {
		t.Fatalf("rules = %+v", rules)
	}
}

func TestWalkFollowsNestedIgnoreFiles(t *testing.T) {
	isolateGitConfig(t)
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".gitignore":            "*.log\n!keep.log\n/build/\ntmp/\n",
		"app.go":                "",
		"debug.log":             "",
		"keep.log":              "",
		"build/out.bin":         "",
		"src/build/main.go":     "",
		"src/tmp/x":             "",
		"src/.gitignore":        "secret.txt\n!debug2.log\n",
		"src/secret.txt":        "",
		"src/debug2.log":        "",
		"src/lib/secret.txt":    "",
		"src/lib/.ignore":       "!secret.txt\n",
		"node_modules/pkg/a.js": "",
		".git/config":           "",
		"docs/.gitignore":       "*\n!.gitignore\n!*.md\n",
		"docs/guide.md":         "",
		"docs/draft.txt":        "",
		"other/secret.txt":      "",
		"other/deep/keep.log":   "",
		"other/deep/.gitignore": "keep.log\n",
	})
	got := strings.Join(walked(t, root), ",")
	want := strings.Join([]string{
		".gitignore", "app.go", "docs/.gitignore", "docs/guide.md", "keep.log", "other/deep/.gitignore", "other/secret.txt",
		"src/.gitignore", "src/build/main.go", "src/debug2.log", "src/lib/.ignore", "src/lib/secret.txt",
	}, ",")
	if got != want {
		t.Fatalf("walked\n got %s\nwant %s", got, want)
	}
}

func TestWalkAppliesRepositoryRulesAboveTheSearchRoot(t *testing.T) {
	home := isolateGitConfig(t)
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		".git/info/exclude":      "*.local\n",
		".gitignore":             "/pkg/sub/generated/\n*.tmp\n",
		"pkg/.gitignore":         "*.cache\n",
		"pkg/sub/a.go":           "",
		"pkg/sub/b.tmp":          "",
		"pkg/sub/c.cache":        "",
		"pkg/sub/d.local":        "",
		"pkg/sub/e.global":       "",
		"pkg/sub/generated/x.go": "",
	})
	writeTree(t, home, map[string]string{".gitconfig": "[user]\n\tname = x\n[core]\n\texcludesFile = ~/global-ignore\n", "global-ignore": "*.global\n"})
	if got := strings.Join(walked(t, filepath.Join(repo, "pkg", "sub")), ","); got != "a.go" {
		t.Fatalf("walked = %s", got)
	}
}

func TestGrepAndFindSkipIgnoredFiles(t *testing.T) {
	isolateGitConfig(t)
	tools, dir := newTools(t, All...)
	writeTree(t, dir, map[string]string{
		".gitignore":      "dist/\n*.min.js\n",
		"src/app.js":      "needle\n",
		"src/app.min.js":  "needle\n",
		"dist/bundle.js":  "needle\n",
		"sub/.gitignore":  "!*.min.js\n",
		"sub/kept.min.js": "needle\n",
	})
	grep := call(t, tools, "grep", map[string]any{"pattern": "needle"})
	if strings.Contains(grep.Content, "dist/") || strings.Contains(grep.Content, "src/app.min.js") || !strings.Contains(grep.Content, "sub/kept.min.js") {
		t.Fatalf("grep = %s", grep.Content)
	}
	find := call(t, tools, "find", map[string]any{"pattern": "**/*.js"})
	if find.Content != "src/app.js\nsub/kept.min.js" {
		t.Fatalf("find = %q", find.Content)
	}
}
