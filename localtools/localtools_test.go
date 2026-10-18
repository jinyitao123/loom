package localtools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
)

func call(t *testing.T, tools *Tools, name string, args any) *contract.ToolResult {
	t.Helper()
	raw, _ := json.Marshal(args)
	result, err := tools.Dispatch(context.Background(), contract.ToolCall{ID: "c", Name: name, Args: string(raw)})
	if err != nil {
		t.Fatalf("%s: Dispatch error %v", name, err)
	}
	return result
}

func newTools(t *testing.T, names ...string) (*Tools, string) {
	t.Helper()
	dir := t.TempDir()
	tools, err := New(dir, names...)
	if err != nil {
		t.Fatal(err)
	}
	return tools, dir
}

func TestListToolsMarksReadOnlyTools(t *testing.T) {
	tools, _ := newTools(t, All...)
	defs, _ := tools.ListTools(context.Background())
	readOnly := map[string]bool{}
	for _, d := range defs {
		readOnly[d.Name] = d.ReadOnly
		if !json.Valid(d.InputSchema) {
			t.Errorf("%s schema is not valid JSON", d.Name)
		}
	}
	for name, want := range map[string]bool{"read": true, "ls": true, "grep": true, "find": true, "write": false, "edit": false, "bash": false} {
		if readOnly[name] != want {
			t.Errorf("%s ReadOnly = %v, want %v", name, readOnly[name], want)
		}
	}
	if _, err := New(t.TempDir(), "nope"); err == nil {
		t.Error("unknown tool name accepted")
	}
	only, _ := newTools(t, "read")
	if r := call(t, only, "write", map[string]any{"path": "x", "content": "y"}); !r.IsError {
		t.Error("disabled tool ran")
	}
}

func TestWriteReadAndPaging(t *testing.T) {
	tools, dir := newTools(t, All...)
	var b strings.Builder
	for i := 1; i <= 2500; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if r := call(t, tools, "write", map[string]any{"path": "sub/a.txt", "content": b.String()}); r.IsError {
		t.Fatal(r.Content)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "a.txt")); err != nil {
		t.Fatal(err)
	}
	first := call(t, tools, "read", map[string]any{"path": "sub/a.txt"})
	if first.IsError || !strings.HasPrefix(first.Content, "line 1\n") || !strings.Contains(first.Content, "Use offset=") {
		t.Fatalf("first page: %q", first.Content[len(first.Content)-80:])
	}
	page := call(t, tools, "read", map[string]any{"path": "sub/a.txt", "offset": 2400, "limit": 2})
	if !strings.HasPrefix(page.Content, "line 2400\nline 2401\n") || !strings.Contains(page.Content, "[Showing lines 2400-2401 of 2500. Use offset=2402 to continue.]") {
		t.Fatalf("page: %q", page.Content)
	}
	tail := call(t, tools, "read", map[string]any{"path": filepath.Join(dir, "sub", "a.txt"), "offset": 2500})
	if tail.Content != "line 2500\n" {
		t.Fatalf("last line: %q", tail.Content)
	}
	if r := call(t, tools, "read", map[string]any{"path": "missing.txt"}); !r.IsError {
		t.Fatal("missing file read without error")
	}
	if err := os.WriteFile(filepath.Join(dir, "bin"), []byte{0, 1, 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := call(t, tools, "read", map[string]any{"path": "bin"}); !r.IsError || !strings.Contains(r.Content, "binary") {
		t.Fatalf("binary file: %+v", r)
	}
}

func TestEditReplacesUniqueBlocks(t *testing.T) {
	tools, dir := newTools(t, All...)
	path := filepath.Join(dir, "f.txt")
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func() string { data, _ := os.ReadFile(path); return string(data) }

	write("alpha\nbeta\ngamma\n")
	r := call(t, tools, "edit", map[string]any{"path": "f.txt", "edits": []map[string]string{{"oldText": "alpha", "newText": "ALPHA"}, {"oldText": "gamma", "newText": "GAMMA"}}})
	if r.IsError || read() != "ALPHA\nbeta\nGAMMA\n" {
		t.Fatalf("edit: %v %q", r.Content, read())
	}

	for name, edits := range map[string][]map[string]string{
		"not found": {{"oldText": "delta", "newText": "x"}},
		"ambiguous": {{"oldText": "A", "newText": "x"}},
		"overlap":   {{"oldText": "ALPHA\nbeta", "newText": "x"}, {"oldText": "beta\nGAMMA", "newText": "y"}},
		"empty":     {{"oldText": "", "newText": "x"}},
	} {
		if r := call(t, tools, "edit", map[string]any{"path": "f.txt", "edits": edits}); !r.IsError {
			t.Errorf("%s: accepted", name)
		}
	}
	if read() != "ALPHA\nbeta\nGAMMA\n" {
		t.Fatalf("rejected edits changed the file: %q", read())
	}

	write("one\r\ntwo\r\n")
	if r := call(t, tools, "edit", map[string]any{"path": "f.txt", "edits": []map[string]string{{"oldText": "one\ntwo", "newText": "1\n2"}}}); r.IsError {
		t.Fatal(r.Content)
	}
	if read() != "1\r\n2\r\n" {
		t.Fatalf("CRLF file: %q", read())
	}
}

func TestLsGrepAndFindRespectIgnores(t *testing.T) {
	tools, dir := newTools(t, All...)
	files := map[string]string{
		"src/app.ts":          "const x = 1\nconst needle = 2\n",
		"src/app.spec.ts":     "needle in spec\n",
		"src/deep/util.go":    "package util\n// NEEDLE\n",
		"node_modules/m/a.ts": "needle hidden\n",
		"build/out.ts":        "needle built\n",
		".gitignore":          "build/\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ls := call(t, tools, "ls", map[string]any{})
	if !strings.Contains(ls.Content, "src/") || !strings.Contains(ls.Content, ".gitignore") {
		t.Fatalf("ls: %q", ls.Content)
	}

	grep := call(t, tools, "grep", map[string]any{"pattern": "needle"})
	if !strings.Contains(grep.Content, "src/app.ts:2: const needle = 2") || strings.Contains(grep.Content, "node_modules") || strings.Contains(grep.Content, "build/") {
		t.Fatalf("grep: %q", grep.Content)
	}
	insensitive := call(t, tools, "grep", map[string]any{"pattern": "needle", "ignoreCase": true, "glob": "*.go"})
	if strings.TrimSpace(insensitive.Content) != "src/deep/util.go:2: // NEEDLE" {
		t.Fatalf("grep -i --glob: %q", insensitive.Content)
	}
	withContext := call(t, tools, "grep", map[string]any{"pattern": "needle = 2", "literal": true, "context": 1})
	if !strings.Contains(withContext.Content, "src/app.ts-1- const x = 1") {
		t.Fatalf("grep context: %q", withContext.Content)
	}
	if none := call(t, tools, "grep", map[string]any{"pattern": "absent"}); none.Content != "No matches found" {
		t.Fatalf("grep none: %q", none.Content)
	}

	find := call(t, tools, "find", map[string]any{"pattern": "**/*.spec.ts"})
	if strings.TrimSpace(find.Content) != "src/app.spec.ts" {
		t.Fatalf("find: %q", find.Content)
	}
	byName := call(t, tools, "find", map[string]any{"pattern": "*.ts"})
	if byName.Content != "src/app.spec.ts\nsrc/app.ts" {
		t.Fatalf("find by name: %q", byName.Content)
	}
}

func TestBashReportsOutputExitCodeAndTimeout(t *testing.T) {
	tools, dir := newTools(t, All...)
	ok := call(t, tools, "bash", map[string]any{"command": "echo hello"})
	if ok.IsError || strings.TrimSpace(ok.Content) != "hello" {
		t.Fatalf("echo: %+v", ok)
	}
	failed := call(t, tools, "bash", map[string]any{"command": "exit 3"})
	if !failed.IsError || !strings.Contains(failed.Content, "exited with code 3") {
		t.Fatalf("exit 3: %+v", failed)
	}
	if runtime.GOOS != "windows" {
		pwd := call(t, tools, "bash", map[string]any{"command": "pwd -P"})
		want, _ := filepath.EvalSymlinks(dir)
		if strings.TrimSpace(pwd.Content) != want {
			t.Fatalf("pwd = %q, want %q", pwd.Content, want)
		}
		slow := call(t, tools, "bash", map[string]any{"command": "sleep 5", "timeout": 0.3})
		if !slow.IsError || !strings.Contains(slow.Content, "timed out") {
			t.Fatalf("timeout: %+v", slow)
		}
	}
}

func TestBashReportsProgressWhileRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell with sleep")
	}
	tools, _ := newTools(t, All...)
	old := ProgressInterval
	ProgressInterval = 20 * time.Millisecond
	t.Cleanup(func() { ProgressInterval = old })
	var mu sync.Mutex
	var reports []string
	tools.OnProgress(func(callID, output string) {
		mu.Lock()
		defer mu.Unlock()
		if callID != "c" {
			t.Errorf("progress call id = %q", callID)
		}
		reports = append(reports, output)
	})
	result := call(t, tools, "bash", map[string]any{"command": "echo one; sleep 0.3; echo two; sleep 0.3"})
	if result.IsError || result.Content != "one\ntwo\n" {
		t.Fatalf("result = %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 2 || reports[0] != "one\n" || reports[1] != "one\ntwo\n" {
		t.Fatalf("progress = %q, want each new output once", reports)
	}
}

func TestTruncateTailKeepsTheEnd(t *testing.T) {
	var b strings.Builder
	for i := 0; i < MaxLines+10; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}
	text, truncated := truncateTail(b.String())
	if !truncated || !strings.HasSuffix(text, fmt.Sprintf("%d\n", MaxLines+9)) || strings.HasPrefix(text, "0\n") {
		t.Fatalf("truncated=%v head=%q", truncated, text[:20])
	}
}

func TestReadReturnsImagesAsParts(t *testing.T) {
	tools, dir := newTools(t, "read")
	png := []byte("\x89PNG\r\n\x1a\nfake")
	if err := os.WriteFile(filepath.Join(dir, "dot.PNG"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	r := call(t, tools, "read", map[string]any{"path": "dot.PNG"})
	if r.IsError || len(r.Parts) != 2 || r.Parts[1].Type != "image" || r.Parts[1].MediaType != "image/png" {
		t.Fatalf("image read = %+v", r)
	}
	if r.Parts[1].Data != base64.StdEncoding.EncodeToString(png) || !strings.Contains(r.Content, "image/png") {
		t.Fatalf("image data or text: %+v", r)
	}
	if missing := call(t, tools, "read", map[string]any{"path": "nope.jpg"}); !missing.IsError {
		t.Fatal("missing image read without error")
	}
}
