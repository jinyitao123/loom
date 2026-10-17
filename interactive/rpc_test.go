package interactive

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
)

// scriptedLLM streams one scripted response per model round.
type scriptedLLM struct {
	mu      sync.Mutex
	rounds  []contract.ChatResponse
	block   chan struct{} // when set, each round waits until it is closed or the request is cancelled
	seen    [][]contract.Message
	efforts []contract.EffortLevel
}

func (s *scriptedLLM) Chat(context.Context, contract.ChatRequest) (*contract.ChatResponse, error) {
	return nil, fmt.Errorf("Chat should not be called directly")
}

func (s *scriptedLLM) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	s.mu.Lock()
	s.seen = append(s.seen, req.Messages)
	s.efforts = append(s.efforts, req.Effort)
	resp := contract.ChatResponse{Content: "done"}
	if len(s.rounds) > 0 {
		resp, s.rounds = s.rounds[0], s.rounds[1:]
	}
	block := s.block
	s.mu.Unlock()
	ch := make(chan contract.StreamChunk, 4)
	go func() {
		defer close(ch)
		if resp.Content != "" {
			ch <- contract.StreamChunk{Content: resp.Content}
		}
		if block != nil {
			select {
			case <-block:
			case <-ctx.Done():
				ch <- contract.StreamChunk{Err: ctx.Err()}
				return
			}
		}
		ch <- contract.StreamChunk{ToolCalls: resp.ToolCalls, Done: true, FinishReason: "stop", Usage: &contract.Usage{InputTokens: 10, OutputTokens: 2}}
	}()
	return ch, nil
}

type fakeTools struct{}

func (fakeTools) ListTools(context.Context) ([]contract.ToolDef, error) {
	return []contract.ToolDef{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`), ReadOnly: true}}, nil
}

func (fakeTools) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	return &contract.ToolResult{CallID: call.ID, Content: "found " + call.Args}, nil
}

// harness drives a Server over pipes and collects its output lines.
type harness struct {
	t      *testing.T
	in     *io.PipeWriter
	lines  chan map[string]any
	done   chan struct{}
	agent  *Agent
	llm    *scriptedLLM
	nextID int
}

func newHarness(t *testing.T, llm *scriptedLLM, reasoning bool) *harness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	server := NewServer(outW)
	catalog := &Catalog{Models: []Model{{ID: "m", Name: "m", API: "openai-completions", Provider: "p", Reasoning: reasoning, ContextWindow: 1000}}, keys: map[string]string{}}
	agent, err := New(Options{
		Cwd:        "/work",
		SessionDir: t.TempDir(),
		Catalog:    catalog,
		NewLLM:     func(Model, string) (contract.LLM, error) { return llm, nil },
		Tools:      fakeTools{},
		Emit:       server.Emit,
	}, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	server.Attach(agent)
	h := &harness{t: t, in: inW, lines: make(chan map[string]any, 1000), done: make(chan struct{}), agent: agent, llm: llm}
	go func() {
		scanner := bufio.NewScanner(outR)
		scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)
		for scanner.Scan() {
			var v map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &v); err != nil {
				t.Errorf("output is not JSON: %q", scanner.Text())
				continue
			}
			h.lines <- v
		}
	}()
	go func() {
		defer close(h.done)
		_ = server.Serve(inR)
		_ = outW.Close()
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		<-h.done
	})
	return h
}

func (h *harness) send(cmd map[string]any) string {
	h.nextID++
	id := fmt.Sprintf("req-%d", h.nextID)
	cmd["id"] = id
	line, _ := json.Marshal(cmd)
	if _, err := h.in.Write(append(line, '\n')); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// until collects lines up to and including the first that matches.
func (h *harness) until(match func(map[string]any) bool) []map[string]any {
	h.t.Helper()
	var got []map[string]any
	timeout := time.After(10 * time.Second)
	for {
		select {
		case line := <-h.lines:
			got = append(got, line)
			if match(line) {
				return got
			}
		case <-timeout:
			h.t.Fatalf("timed out; received %v", got)
		}
	}
}

func isResponse(id string) func(map[string]any) bool {
	return func(v map[string]any) bool { return v["type"] == "response" && v["id"] == id }
}

func isType(kind string) func(map[string]any) bool {
	return func(v map[string]any) bool { return v["type"] == kind }
}

func types(lines []map[string]any) []string {
	var out []string
	for _, l := range lines {
		kind := l["type"].(string)
		if kind == "message_update" {
			kind += "/" + l["assistantMessageEvent"].(map[string]any)["type"].(string)
		}
		if kind == "message_start" || kind == "message_end" {
			kind += "/" + l["message"].(map[string]any)["role"].(string)
		}
		out = append(out, kind)
	}
	return out
}

func TestServerStreamsAPromptWithAToolRound(t *testing.T) {
	llm := &scriptedLLM{rounds: []contract.ChatResponse{
		{Content: "Let me check.", ToolCalls: []contract.ToolCall{{ID: "c1", Name: "lookup", Args: `{"q":"x"}`}}},
		{Content: "It is x."},
	}}
	h := newHarness(t, llm, false)

	state := h.send(map[string]any{"type": "get_state"})
	resp := h.until(isResponse(state))
	data := resp[len(resp)-1]["data"].(map[string]any)
	if data["isStreaming"] != false || data["model"].(map[string]any)["id"] != "m" {
		t.Fatalf("get_state = %v", data)
	}

	h.send(map[string]any{"type": "prompt", "message": "what is x?"})
	events := h.until(isType("agent_settled"))
	want := []string{
		"response", "agent_start", "turn_start", "message_start/user", "message_end/user",
		"message_start/assistant", "message_update/text_start", "message_update/text_delta", "message_update/text_end", "message_end/assistant",
		"tool_execution_start", "tool_execution_end", "message_start/toolResult", "message_end/toolResult",
		"turn_end", "turn_start",
		"message_start/assistant", "message_update/text_start", "message_update/text_delta", "message_update/text_end", "message_end/assistant",
		"turn_end", "agent_end", "agent_settled",
	}
	if got := types(events); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event order:\n got %v\nwant %v", got, want)
	}
	toolEnd := events[11]
	if toolEnd["toolCallId"] != "c1" || toolEnd["isError"] != false {
		t.Fatalf("tool_execution_end = %v", toolEnd)
	}
	firstAssistant := events[9]["message"].(map[string]any)
	if firstAssistant["stopReason"] != "toolUse" || len(firstAssistant["content"].([]any)) != 2 {
		t.Fatalf("assistant with tool call = %v", firstAssistant)
	}
	if llm.efforts[0] != "" {
		t.Fatalf("effort sent to a model without reasoning: %q", llm.efforts[0])
	}

	// The second round saw the tool result; the session holds all five messages.
	if second := llm.seen[1]; second[len(second)-1].Role != "tool" || second[len(second)-1].Content != `found {"q":"x"}` {
		t.Fatalf("second round history = %+v", second)
	}
	reopened, err := OpenSession(h.agent.State()["sessionFile"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range reopened.Context().Messages {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "user,assistant,toolResult,assistant" {
		t.Fatalf("persisted roles = %v", roles)
	}

	text := h.send(map[string]any{"type": "get_last_assistant_text"})
	last := h.until(isResponse(text))
	if got := last[len(last)-1]["data"].(map[string]any)["text"]; got != "It is x." {
		t.Fatalf("last assistant text = %v", got)
	}
	stats := h.send(map[string]any{"type": "get_session_stats"})
	statLines := h.until(isResponse(stats))
	s := statLines[len(statLines)-1]["data"].(map[string]any)
	if s["toolCalls"].(float64) != 1 || s["assistantMessages"].(float64) != 2 || s["tokens"].(map[string]any)["input"].(float64) != 20 {
		t.Fatalf("stats = %v", s)
	}
}

func TestServerRejectsPromptWhileBusyAndAbortKeepsPartialText(t *testing.T) {
	llm := &scriptedLLM{block: make(chan struct{}), rounds: []contract.ChatResponse{{Content: "partial"}}}
	h := newHarness(t, llm, true)

	h.send(map[string]any{"type": "prompt", "message": "long task"})
	h.until(func(v map[string]any) bool {
		return v["type"] == "message_update" && v["assistantMessageEvent"].(map[string]any)["type"] == "text_delta"
	})
	busy := h.send(map[string]any{"type": "prompt", "message": "another"})
	resp := h.until(isResponse(busy))
	if resp[len(resp)-1]["success"] != false {
		t.Fatalf("prompt while busy was accepted: %v", resp[len(resp)-1])
	}
	queued := h.send(map[string]any{"type": "prompt", "message": "later", "streamingBehavior": "followUp"})
	lines := h.until(isResponse(queued))
	if lines[len(lines)-1]["success"] != true || types(lines)[0] != "queue_update" {
		t.Fatalf("follow-up not queued: %v", lines)
	}

	abort := h.send(map[string]any{"type": "abort"})
	lines = h.until(isResponse(abort))
	var aborted map[string]any
	for _, l := range lines {
		if l["type"] == "message_end" && l["message"].(map[string]any)["role"] == "assistant" {
			aborted = l["message"].(map[string]any)
		}
	}
	if aborted == nil || aborted["stopReason"] != "aborted" {
		t.Fatalf("no aborted assistant message in %v", types(lines))
	}
	if content := aborted["content"].([]any); len(content) != 1 || content[0].(map[string]any)["text"] != "partial" {
		t.Fatalf("partial text lost: %v", aborted["content"])
	}
	if llm.efforts[0] != contract.EffortMedium {
		t.Fatalf("effort for a reasoning model at medium = %q", llm.efforts[0])
	}
	state := h.send(map[string]any{"type": "get_state"})
	lines = h.until(isResponse(state))
	data := lines[len(lines)-1]["data"].(map[string]any)
	if data["isStreaming"] != false || data["pendingMessageCount"].(float64) != 0 {
		t.Fatalf("state after abort = %v", data)
	}
}

func TestServerReportsUnknownAndUnsupportedCommands(t *testing.T) {
	h := newHarness(t, &scriptedLLM{}, false)
	for kind, want := range map[string]string{"compact": "not supported", "nonsense": "Unknown command"} {
		id := h.send(map[string]any{"type": kind})
		lines := h.until(isResponse(id))
		last := lines[len(lines)-1]
		if last["success"] != false || !strings.Contains(last["error"].(string), want) {
			t.Fatalf("%s: %v", kind, last)
		}
	}
	if _, err := h.in.Write([]byte("{not json\n")); err != nil {
		t.Fatal(err)
	}
	lines := h.until(func(v map[string]any) bool { return v["command"] == "parse" })
	if lines[len(lines)-1]["success"] != false {
		t.Fatalf("parse error = %v", lines[len(lines)-1])
	}
}

func TestServerSwitchesModelsAndNamesSessions(t *testing.T) {
	h := newHarness(t, &scriptedLLM{}, false)
	h.agent.opts.Catalog.Models = append(h.agent.opts.Catalog.Models, Model{ID: "m2", Name: "m2", API: "openai-completions", Provider: "p", Reasoning: true})

	set := h.send(map[string]any{"type": "set_model", "provider": "p", "modelId": "m2"})
	lines := h.until(isResponse(set))
	if lines[len(lines)-1]["data"].(map[string]any)["id"] != "m2" {
		t.Fatalf("set_model = %v", lines[len(lines)-1])
	}
	missing := h.send(map[string]any{"type": "set_model", "provider": "p", "modelId": "nope"})
	lines = h.until(isResponse(missing))
	if lines[len(lines)-1]["success"] != false {
		t.Fatalf("unknown model accepted: %v", lines[len(lines)-1])
	}
	name := h.send(map[string]any{"type": "set_session_name", "name": "Refactor\nauth"})
	h.until(isResponse(name))
	state := h.send(map[string]any{"type": "get_state"})
	lines = h.until(isResponse(state))
	data := lines[len(lines)-1]["data"].(map[string]any)
	if data["sessionName"] != "Refactor auth" || data["model"].(map[string]any)["id"] != "m2" {
		t.Fatalf("state = %v", data)
	}
	levels := h.send(map[string]any{"type": "get_available_thinking_levels"})
	lines = h.until(isResponse(levels))
	if got := lines[len(lines)-1]["data"].(map[string]any)["levels"].([]any); len(got) != 5 {
		t.Fatalf("levels for a reasoning model = %v", got)
	}
}
