package interactive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContextFilesFromUserAndAncestors(t *testing.T) {
	root := t.TempDir()
	agentDir := t.TempDir()
	cwd := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(agentDir, "AGENTS.md"), "user rules")
	write(filepath.Join(agentDir, "CLAUDE.md"), "not read")
	write(filepath.Join(root, "AGENTS.md"), "root rules")
	write(filepath.Join(root, "a", "CLAUDE.md"), "claude rules")
	write(filepath.Join(cwd, "AGENTS.md"), "local rules")
	write(filepath.Join(cwd, "CLAUDE.md"), "shadowed by AGENTS.md")

	var got []string
	for _, f := range LoadContextFiles(agentDir, cwd) {
		// Directories above the temporary root are not part of the test.
		if strings.HasPrefix(f.Path, root) || strings.HasPrefix(f.Path, agentDir) {
			got = append(got, f.Content)
		}
	}
	if strings.Join(got, "|") != "user rules|root rules|claude rules|local rules" {
		t.Fatalf("context files = %q", got)
	}

	write(filepath.Join(cwd, "AGENTS.md"), strings.Repeat("x", MaxContextFile+1))
	for _, f := range LoadContextFiles("", cwd) {
		if f.Path == filepath.Join(cwd, "AGENTS.md") || f.Path == filepath.Join(cwd, "CLAUDE.md") {
			t.Fatalf("an oversized AGENTS.md is skipped without falling back: %s", f.Path)
		}
	}

	prompt := ContextPrompt([]ContextFile{{Path: "/p/AGENTS.md", Content: "rule one\n"}})
	if !strings.Contains(prompt, "## /p/AGENTS.md\n\nrule one\n") || ContextPrompt(nil) != "" {
		t.Fatalf("prompt = %q", prompt)
	}
}
