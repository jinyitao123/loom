package interactive

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
)

// withRetryDelay sets the first retry backoff for one test.
func withRetryDelay(t *testing.T, d time.Duration) {
	old := retryBaseDelay
	retryBaseDelay = d
	t.Cleanup(func() { retryBaseDelay = old })
}

func storedRoles(a *Agent) string {
	var roles []string
	for _, m := range a.Messages() {
		roles = append(roles, m.Role)
	}
	return strings.Join(roles, ",")
}

func TestTransientFailureIsRetried(t *testing.T) {
	withRetryDelay(t, time.Millisecond)
	llm := &scriptedLLM{fail: []error{errors.New("HTTP 503: overloaded")}}
	h := newHarness(t, llm, false)
	h.send(map[string]any{"type": "prompt", "message": "hi"})
	events := h.until(isType("agent_settled"))

	var start, end map[string]any
	for _, e := range events {
		switch e["type"] {
		case "auto_retry_start":
			start = e
		case "auto_retry_end":
			end = e
		}
	}
	if start == nil || start["attempt"] != float64(1) || !strings.Contains(start["errorMessage"].(string), "503") {
		t.Fatalf("auto_retry_start = %v", start)
	}
	if end == nil || end["success"] != true {
		t.Fatalf("auto_retry_end = %v", end)
	}
	if got := storedRoles(h.agent); got != "user,assistant" {
		t.Fatalf("stored roles = %s (the failed attempt must not be stored)", got)
	}
	if text := h.agent.LastAssistantText(); text == nil || *text != "done" {
		t.Fatalf("answer after retry = %v", text)
	}
}

func TestAbortRetryEndsThePromptWithTheError(t *testing.T) {
	withRetryDelay(t, time.Hour)
	llm := &scriptedLLM{fail: []error{errors.New("429 Too Many Requests: rate limit")}}
	h := newHarness(t, llm, false)
	h.send(map[string]any{"type": "prompt", "message": "hi"})
	h.until(isType("auto_retry_start"))
	h.send(map[string]any{"type": "abort_retry"})
	events := h.until(isType("agent_settled"))

	var end map[string]any
	for _, e := range events {
		if e["type"] == "auto_retry_end" {
			end = e
		}
	}
	if end == nil || end["success"] != false || !strings.Contains(end["finalError"].(string), "rate limit") {
		t.Fatalf("auto_retry_end = %v", end)
	}
	messages := h.agent.Messages()
	last := messages[len(messages)-1]
	if last.Role != "assistant" || last.StopReason != "error" || !strings.Contains(last.ErrorMessage, "rate limit") {
		t.Fatalf("last message = %+v", last)
	}
	if got := storedRoles(h.agent); got != "user,assistant" {
		t.Fatalf("stored roles = %s (the error must be stored once)", got)
	}
}

func TestDisabledAutoRetryAndPermanentErrorsFailAtOnce(t *testing.T) {
	withRetryDelay(t, time.Millisecond)
	llm := &scriptedLLM{fail: []error{errors.New("HTTP 500"), errors.New("HTTP 401: invalid api key")}}
	h := newHarness(t, llm, false)
	h.until(isResponse(h.send(map[string]any{"type": "set_auto_retry", "enabled": false})))
	h.send(map[string]any{"type": "prompt", "message": "one"})
	first := h.until(isType("agent_settled"))
	h.until(isResponse(h.send(map[string]any{"type": "set_auto_retry", "enabled": true})))
	h.send(map[string]any{"type": "prompt", "message": "two"})
	second := h.until(isType("agent_settled"))

	for _, e := range append(first, second...) {
		if e["type"] == "auto_retry_start" {
			t.Fatalf("unexpected retry: %v", e)
		}
	}
	if got := len(llm.seen); got != 2 {
		t.Fatalf("model rounds = %d, want 2", got)
	}
}

func TestIsTransient(t *testing.T) {
	for _, tc := range []struct {
		err  string
		want bool
	}{
		{"HTTP 429 Too Many Requests", true},
		{"HTTP 502 Bad Gateway", true},
		{"anthropic: overloaded_error", true},
		{"read tcp: connection reset by peer", true},
		{"stream scanner: unexpected EOF", true},
		{"HTTP 400: invalid request", false},
		{"HTTP 401: unauthorized", false},
		{"This model's maximum context length is 128000 tokens", false},
	} {
		if got := IsTransient(errors.New(tc.err)); got != tc.want {
			t.Errorf("IsTransient(%q) = %v, want %v", tc.err, got, tc.want)
		}
	}
	if IsTransient(context.Canceled) {
		t.Error("cancellation must not be retried")
	}
}

// progressTools reports partial output through the agent while it runs.
type progressTools struct {
	fakeTools
	agent func() *Agent
}

func (p *progressTools) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	a := p.agent()
	a.ToolProgress("unknown-call", "ignored")
	a.ToolProgress(call.ID, "line 1")
	a.ToolProgress(call.ID, "line 1\nline 2")
	return p.fakeTools.Dispatch(ctx, call)
}

func TestToolProgressIsReported(t *testing.T) {
	llm := &scriptedLLM{rounds: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "c1", Name: "lookup", Args: `{"q":"x"}`}}},
		{Content: "ok"},
	}}
	var h *harness
	tools := &progressTools{agent: func() *Agent { return h.agent }}
	h = newHarnessWith(t, llm, func(o *Options, _ *Model) { o.Tools = tools })
	h.send(map[string]any{"type": "prompt", "message": "go"})
	events := h.until(isType("agent_settled"))

	var updates []map[string]any
	var order []string
	for _, e := range events {
		switch e["type"] {
		case "tool_execution_update":
			updates = append(updates, e)
			order = append(order, "update")
		case "tool_execution_start", "tool_execution_end":
			order = append(order, e["type"].(string))
		}
	}
	if strings.Join(order, ",") != "tool_execution_start,update,update,tool_execution_end" {
		t.Fatalf("tool events = %v", order)
	}
	last := updates[1]
	if last["toolCallId"] != "c1" || last["toolName"] != "lookup" || last["args"].(map[string]any)["q"] != "x" {
		t.Fatalf("update = %v", last)
	}
	content := last["partialResult"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if content["text"] != "line 1\nline 2" {
		t.Fatalf("partial output = %v", content)
	}
}

func TestTurnStartHookSeesThePromptBeforeTheModel(t *testing.T) {
	var mu sync.Mutex
	var prompts []string
	llm := &scriptedLLM{}
	h := newHarnessWith(t, llm, func(o *Options, _ *Model) {
		o.OnTurnStart = func(_ context.Context, prompt string) error {
			mu.Lock()
			defer mu.Unlock()
			if len(llm.seen) != len(prompts) {
				t.Errorf("turn hook ran after the model")
			}
			prompts = append(prompts, prompt)
			if prompt == "fail" {
				return errors.New("server unavailable")
			}
			return nil
		}
	})
	h.send(map[string]any{"type": "prompt", "message": "first"})
	h.until(isType("agent_settled"))
	h.send(map[string]any{"type": "prompt", "message": "fail"})
	events := h.until(isType("agent_settled"))

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(prompts, ",") != "first,fail" {
		t.Fatalf("turn hook prompts = %v", prompts)
	}
	var reported bool
	for _, e := range events {
		if e["type"] == "extension_error" && e["extensionPath"] == "turn" {
			reported = true
		}
	}
	if !reported {
		t.Fatal("a failing turn hook must be reported")
	}
	if text := h.agent.LastAssistantText(); text == nil || *text != "done" {
		t.Fatalf("the turn must continue after a hook error: %v", text)
	}
}

func TestRoundBudgetEndsTheTurn(t *testing.T) {
	call := contract.ChatResponse{ToolCalls: []contract.ToolCall{{ID: "c1", Name: "lookup", Args: "{}"}}}
	llm := &scriptedLLM{rounds: []contract.ChatResponse{call, call, call}}
	h := newHarnessWith(t, llm, func(o *Options, _ *Model) { o.MaxIterations = 2 })
	h.send(map[string]any{"type": "prompt", "message": "loop"})
	h.until(isType("agent_settled"))

	if got := len(llm.seen); got != 2 {
		t.Fatalf("model rounds = %d, want 2", got)
	}
	if got := storedRoles(h.agent); got != "user,assistant,toolResult,assistant,toolResult" {
		t.Fatalf("stored roles = %s", got)
	}
}
