package interactive

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

const pngData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

func lastUser(messages []contract.Message) contract.Message {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i]
		}
	}
	return contract.Message{}
}

func TestPromptImagesReachVisionModelsOnly(t *testing.T) {
	image := map[string]any{"type": "image", "data": pngData, "mimeType": "image/png"}

	vision := newHarnessWith(t, &scriptedLLM{}, func(_ *Options, m *Model) { m.Input = []string{"text", "image"} })
	vision.send(map[string]any{"type": "prompt", "message": "what is this?", "images": []any{image}})
	vision.until(isType("agent_settled"))
	sent := lastUser(vision.llm.seen[0])
	if len(sent.Parts) != 2 || sent.Parts[1].Type != "image" || sent.Parts[1].Data != pngData || sent.Parts[1].MediaType != "image/png" {
		t.Fatalf("vision request parts = %+v", sent.Parts)
	}
	if stored := vision.agent.Messages()[0]; len(stored.Content) != 2 || stored.Content[1].Type != "image" {
		t.Fatalf("stored user message = %+v", stored.Content)
	}

	text := newHarnessWith(t, &scriptedLLM{}, func(*Options, *Model) {})
	text.send(map[string]any{"type": "prompt", "message": "what is this?", "images": []any{image}})
	text.until(isType("agent_settled"))
	sent = lastUser(text.llm.seen[0])
	if sent.Parts != nil || !strings.Contains(sent.Content, "does not accept images") {
		t.Fatalf("text-only request = %+v", sent)
	}

	bad := text.send(map[string]any{"type": "prompt", "message": "x", "images": []any{map[string]any{"type": "image", "data": "@@", "mimeType": "image/png"}}})
	lines := text.until(isResponse(bad))
	if lines[len(lines)-1]["success"] != false {
		t.Fatalf("invalid image accepted: %v", lines[len(lines)-1])
	}
}

func TestToolResultImagesAreStoredAndSent(t *testing.T) {
	llm := &scriptedLLM{rounds: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "c1", Name: "lookup", Args: "{}"}}},
		{Content: "a red dot"},
	}}
	h := newHarnessWith(t, llm, func(o *Options, m *Model) {
		m.Input = []string{"text", "image"}
		o.Tools = &fakeTools{parts: []contract.ContentPart{{Type: "text", Text: "Read image dot.png"}, {Type: "image", MediaType: "image/png", Data: pngData}}}
	})
	h.send(map[string]any{"type": "prompt", "message": "describe dot.png"})
	events := h.until(isType("agent_settled"))
	var end map[string]any
	for _, e := range events {
		if e["type"] == "tool_execution_end" {
			end = e
		}
	}
	content := end["result"].(map[string]any)["content"].([]any)
	if len(content) != 2 || content[1].(map[string]any)["type"] != "image" {
		t.Fatalf("tool_execution_end content = %v", content)
	}
	second := llm.seen[1]
	if tool := second[len(second)-1]; tool.Role != "tool" || len(tool.Parts) != 2 {
		t.Fatalf("tool message sent to the model = %+v", tool)
	}
}

func TestSteeringIsDeliveredBetweenRounds(t *testing.T) {
	llm := &scriptedLLM{rounds: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "c1", Name: "lookup", Args: "{}"}}},
		{Content: "done with both"},
	}}
	tools := &fakeTools{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	h := newHarnessWith(t, llm, func(o *Options, _ *Model) { o.Tools = tools })
	h.send(map[string]any{"type": "prompt", "message": "check x"})
	<-tools.entered
	steer := h.send(map[string]any{"type": "steer", "message": "also check y"})
	h.until(isResponse(steer))
	close(tools.gate)
	events := h.until(isType("agent_settled"))

	if got := len(llm.seen); got != 2 {
		t.Fatalf("model rounds = %d, want 2 (steering must not start another prompt)", got)
	}
	second := llm.seen[1]
	if last := second[len(second)-1]; last.Role != "user" || last.Content != "also check y" {
		t.Fatalf("second round did not receive the steering message last: %+v", second)
	}
	var order []string
	for _, kind := range types(events) {
		if kind == "message_start/user" || kind == "message_start/assistant" || kind == "agent_end" {
			order = append(order, kind)
		}
	}
	if strings.Join(order, ",") != "message_start/user,message_start/assistant,agent_end" {
		t.Fatalf("order after steering = %v", order)
	}
	roles := []string{}
	for _, m := range h.agent.Messages() {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "user,assistant,toolResult,user,assistant" {
		t.Fatalf("stored roles = %v", roles)
	}
}

// compactionHarness has a small window: compaction starts above 800 tokens
// and keeps the newest 50 tokens verbatim.
func compactionHarness(t *testing.T, llm *scriptedLLM) *harness {
	return newHarnessWith(t, llm, func(o *Options, m *Model) {
		m.ContextWindow = 1000
		o.ReserveTokens = 200
		o.KeepRecentTokens = 50
	})
}

func TestThresholdCompactionSummarizesOlderHistory(t *testing.T) {
	llm := &scriptedLLM{}
	h := compactionHarness(t, llm)
	h.send(map[string]any{"type": "prompt", "message": strings.Repeat("a", 2400)})
	h.until(isType("agent_settled"))
	h.send(map[string]any{"type": "prompt", "message": "second " + strings.Repeat("b", 2400)})
	events := h.until(isType("agent_settled"))

	var start, end map[string]any
	for _, e := range events {
		switch e["type"] {
		case "compaction_start":
			start = e
		case "compaction_end":
			end = e
		}
	}
	if start == nil || start["reason"] != "threshold" || end == nil || end["result"] == nil {
		t.Fatalf("compaction events: start=%v end=%v", start, end)
	}
	if len(llm.summary) != 1 || !strings.Contains(llm.summary[0].Messages[1].Content, "[User]: aaaa") {
		t.Fatalf("summarizer input = %+v", llm.summary)
	}
	sent := llm.seen[len(llm.seen)-1]
	joined := ""
	for _, m := range sent {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "SUMMARY") || strings.Contains(joined, strings.Repeat("a", 100)) || !strings.Contains(joined, "second ") {
		t.Fatalf("request after compaction:\n%s", joined[:min(len(joined), 400)])
	}

	reopened, err := OpenSession(h.agent.State()["sessionFile"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var compaction *Entry
	for _, e := range reopened.Entries() {
		if e.Type == "compaction" {
			compaction = e
		}
	}
	if compaction == nil || compaction.FirstKeptEntryID == "" || compaction.Usage == nil {
		t.Fatalf("compaction entry = %+v", compaction)
	}
	if kept := reopened.byID[compaction.FirstKeptEntryID]; kept == nil || !strings.HasPrefix(kept.Message.Text(), "second ") {
		t.Fatalf("first kept entry = %+v", kept)
	}
	if ctx := reopened.Context(); ctx.Messages[0].Role != "compactionSummary" {
		t.Fatalf("reopened context starts with %s", ctx.Messages[0].Role)
	}
}

func TestManualCompactionAndBusyRejection(t *testing.T) {
	llm := &scriptedLLM{}
	h := newHarnessWith(t, llm, func(o *Options, _ *Model) { o.KeepRecentTokens = 50 })
	empty := h.send(map[string]any{"type": "compact"})
	lines := h.until(isResponse(empty))
	if lines[len(lines)-1]["success"] != false {
		t.Fatalf("compacting an empty session succeeded: %v", lines[len(lines)-1])
	}
	for _, text := range []string{strings.Repeat("a", 800), strings.Repeat("b", 800)} {
		h.send(map[string]any{"type": "prompt", "message": text})
		h.until(isType("agent_settled"))
	}
	id := h.send(map[string]any{"type": "compact", "customInstructions": "focus on b"})
	lines = h.until(isResponse(id))
	resp := lines[len(lines)-1]
	if resp["success"] != true || !strings.Contains(resp["data"].(map[string]any)["summary"].(string), "SUMMARY") {
		t.Fatalf("compact response = %v", resp)
	}
	if !strings.Contains(llm.summary[0].Messages[1].Content, "Additional focus: focus on b") {
		t.Fatal("custom instructions were not passed to the summarizer")
	}
	state := h.agent.State()
	if state["isCompacting"] != false || state["isStreaming"] != false {
		t.Fatalf("state after compaction = %v", state)
	}

	busyLLM := &scriptedLLM{block: make(chan struct{})}
	busy := newHarness(t, busyLLM, false)
	busy.send(map[string]any{"type": "prompt", "message": "long"})
	busy.until(func(v map[string]any) bool { return v["type"] == "message_update" })
	id = busy.send(map[string]any{"type": "compact"})
	lines = busy.until(isResponse(id))
	if lines[len(lines)-1]["success"] != false {
		t.Fatalf("compaction while working succeeded: %v", lines[len(lines)-1])
	}
	close(busyLLM.block)
	busy.until(isType("agent_settled"))
}

func TestContextOverflowCompactsAndRetries(t *testing.T) {
	llm := &scriptedLLM{}
	h := newHarnessWith(t, llm, func(o *Options, _ *Model) { o.KeepRecentTokens = 50 })
	h.send(map[string]any{"type": "prompt", "message": strings.Repeat("a", 800)})
	h.until(isType("agent_settled"))

	llm.mu.Lock()
	llm.fail = []error{errors.New("This model's maximum context length is 128000 tokens")}
	llm.mu.Unlock()
	h.send(map[string]any{"type": "prompt", "message": strings.Repeat("b", 800)})
	events := h.until(isType("agent_settled"))

	var end map[string]any
	for _, e := range events {
		if e["type"] == "compaction_end" {
			end = e
		}
	}
	if end == nil || end["reason"] != "overflow" || end["willRetry"] != true {
		t.Fatalf("compaction_end = %v", end)
	}
	if text := h.agent.LastAssistantText(); text == nil || *text != "done" {
		t.Fatalf("answer after retry = %v", text)
	}
}

func TestSkillCommandExpandsAndIsListed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillFile := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(skillFile, []byte("---\nname: demo\ndescription: \"Demo steps\"\n---\n# Demo\nAlways say hello first.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	skills := LoadSkills("path", filepath.Dir(dir))
	if len(skills) != 1 || skills[0].Name != "demo" || skills[0].Description != "Demo steps" {
		t.Fatalf("skills = %+v", skills)
	}
	if prompt := SkillsPrompt(skills); !strings.Contains(prompt, "<name>demo</name>") || !strings.Contains(prompt, skillFile) {
		t.Fatalf("skills prompt = %q", prompt)
	}

	llm := &scriptedLLM{}
	h := newHarnessWith(t, llm, func(o *Options, _ *Model) { o.Skills = skills })
	cmds := h.send(map[string]any{"type": "get_commands"})
	lines := h.until(isResponse(cmds))
	list := lines[len(lines)-1]["data"].(map[string]any)["commands"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["name"] != "skill:demo" {
		t.Fatalf("commands = %v", list)
	}
	h.send(map[string]any{"type": "prompt", "message": "/skill:demo greet the team"})
	h.until(isType("agent_settled"))
	sent := lastUser(llm.seen[0]).Content
	if !strings.Contains(sent, "Always say hello first.") || !strings.HasSuffix(sent, "greet the team") || strings.Contains(sent, "description:") {
		t.Fatalf("expanded prompt = %q", sent)
	}
}
