package interactive

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionDirReplacesSeparators(t *testing.T) {
	for cwd, want := range map[string]string{
		"/Users/me/project":  "--Users-me-project--",
		`C:\work\project`:    "--C--work-project--",
		"/var/folders/x/T/a": "--var-folders-x-T-a--",
	} {
		if got := filepath.Base(SessionDir("/root", cwd)); got != want {
			t.Errorf("SessionDir(%q) = %q, want %q", cwd, got, want)
		}
	}
}

func TestSessionWritesOnlyAfterFirstAssistantMessage(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(dir, "/work", "")
	if err := s.Append(&Entry{Type: "message", Message: &Message{Role: "user", Content: textBlocks("hi"), Timestamp: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatalf("session file exists before any assistant message: %v", err)
	}
	if err := s.Append(&Entry{Type: "message", Message: &Message{Role: "assistant", Content: textBlocks("hello"), StopReason: "stop", Timestamp: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(&Entry{Type: "message", Message: &Message{Role: "user", Content: textBlocks("again"), Timestamp: 3}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 {
		t.Fatalf("session file has %d lines, want header + 3 entries:\n%s", len(lines), data)
	}
	var header Header
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil || header.Type != "session" || header.Version != 3 || header.Cwd != "/work" {
		t.Fatalf("header = %+v, err %v", header, err)
	}
	if !strings.HasSuffix(s.Path(), "_"+header.ID+".jsonl") {
		t.Fatalf("file name %q does not end with the session id", s.Path())
	}

	reopened, err := OpenSession(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	ctx := reopened.Context()
	if len(ctx.Messages) != 3 || ctx.Messages[2].Text() != "again" {
		t.Fatalf("reopened context = %+v", ctx.Messages)
	}
	entries := reopened.Entries()
	if entries[0].ParentID != nil || *entries[1].ParentID != entries[0].ID || *entries[2].ParentID != entries[1].ID {
		t.Fatal("entries are not chained by parentId")
	}
}

func TestSessionContextHonorsCompactionAndBranches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	lines := []string{
		`{"type":"session","version":3,"id":"sid","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/w"}`,
		`{"type":"message","id":"a1","parentId":null,"timestamp":"t","message":{"role":"user","content":"old question","timestamp":1}}`,
		`{"type":"message","id":"a2","parentId":"a1","timestamp":"t","message":{"role":"assistant","content":[{"type":"text","text":"old answer"}],"provider":"p","model":"m","stopReason":"stop","timestamp":2}}`,
		`{"type":"message","id":"a3","parentId":"a2","timestamp":"t","message":{"role":"user","content":"kept question","timestamp":3}}`,
		`{"type":"compaction","id":"c1","parentId":"a3","timestamp":"t","summary":"talked about X","firstKeptEntryId":"a3","tokensBefore":900}`,
		`{"type":"model_change","id":"m1","parentId":"c1","timestamp":"t","provider":"p2","modelId":"m2"}`,
		`{"type":"message","id":"a4","parentId":"m1","timestamp":"t","message":{"role":"user","content":[{"type":"text","text":"new question"}],"timestamp":4}}`,
		`{"type":"message","id":"b1","parentId":"a1","timestamp":"t","message":{"role":"user","content":"abandoned branch","timestamp":5}}`,
		`{"type":"message","id":"a5","parentId":"a4","timestamp":"t","message":{"role":"assistant","content":[{"type":"text","text":"new answer"}],"provider":"p2","model":"m2","stopReason":"stop","timestamp":6}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := s.Context()
	var got []string
	for _, m := range ctx.Messages {
		if m.Role == "compactionSummary" {
			got = append(got, "summary:"+m.Summary)
			continue
		}
		got = append(got, m.Role+":"+m.Text())
	}
	want := []string{"summary:talked about X", "user:kept question", "user:new question", "assistant:new answer"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("context = %v, want %v", got, want)
	}
	if ctx.Provider != "p2" || ctx.ModelID != "m2" {
		t.Fatalf("model = %s/%s", ctx.Provider, ctx.ModelID)
	}
}

func TestToContractAnswersEveryToolCall(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: []Block{{Type: "text", Text: "look"}, {Type: "image", Data: "AAAA", MimeType: "image/png"}}},
		{Role: "assistant", Content: []Block{{Type: "toolCall", ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a"}`)}, {Type: "toolCall", ID: "c2", Name: "read"}}, StopReason: "aborted"},
		{Role: "toolResult", ToolCallID: "c1", ToolName: "read", Content: textBlocks("contents")},
		{Role: "assistant", StopReason: "aborted"},
	}
	out := toContract(messages, Model{})
	if len(out) != 4 {
		t.Fatalf("got %d messages: %+v", len(out), out)
	}
	if !strings.Contains(out[0].Content, "image") {
		t.Errorf("image note missing: %q", out[0].Content)
	}
	if len(out[1].ToolCalls) != 2 || out[1].ToolCalls[1].Args != "{}" {
		t.Errorf("tool calls = %+v", out[1].ToolCalls)
	}
	if m := out[2]; m.Role != "tool" || m.ToolCallID != "c2" || m.Content != "Tool call was not completed." {
		t.Errorf("synthetic result = %+v", out[2])
	}
	if out[3].Role != "tool" || out[3].ToolCallID != "c1" {
		t.Errorf("real result = %+v", out[3])
	}
}

func TestLoadCatalogPrefersAuthFileAndEnvironment(t *testing.T) {
	dir := t.TempDir()
	models := `{"providers":{"local":{"baseUrl":"http://127.0.0.1:1/v1","api":"openai-completions","apiKey":"LOCAL_KEY","models":[{"id":"m1"},{"id":"m2","reasoning":true,"contextWindow":1000}]},"other":{"baseUrl":"http://x/v1","apiKey":"literal","models":[{"id":"o1"}]}}}`
	auth := `{"other":{"type":"api_key","key":"from-auth"}}`
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadCatalog(dir, func(name string) string {
		if name == "LOCAL_KEY" {
			return "from-env"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 3 || catalog.Models[0].Provider != "local" || catalog.Models[0].Name != "m1" {
		t.Fatalf("models = %+v", catalog.Models)
	}
	if m, ok := catalog.Find("local", "m2"); !ok || !m.Reasoning || m.ContextWindow != 1000 || m.BaseURL != "http://127.0.0.1:1/v1" {
		t.Fatalf("m2 = %+v, %v", m, ok)
	}
	if catalog.Key("local") != "from-env" || catalog.Key("other") != "from-auth" {
		t.Fatalf("keys: local=%q other=%q", catalog.Key("local"), catalog.Key("other"))
	}
	empty, err := LoadCatalog(t.TempDir(), func(string) string { return "" })
	if err != nil || len(empty.Models) != 0 {
		t.Fatalf("empty catalog = %+v, %v", empty, err)
	}
}
