package stdlib_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/openai"
	"github.com/jinyitao123/loom/stdlib"
)

type recordedMessage struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ToolCallID       string `json:"tool_call_id"`
	ReasoningContent string `json:"reasoning_content"`
	ReasoningBytes   int    `json:"recording_reasoning_bytes"`
	ReasoningSHA256  string `json:"recording_reasoning_sha256"`
	ToolCalls        []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func conversation(messages []recordedMessage) []contract.Message {
	result := make([]contract.Message, len(messages))
	for i, message := range messages {
		result[i] = contract.Message{Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			result[i].ToolCalls = append(result[i].ToolCalls, contract.ToolCall{ID: call.ID, Name: call.Function.Name, Args: call.Function.Arguments})
		}
	}
	return result
}

type recordedTurn struct {
	Status  int `json:"status"`
	Request struct {
		Model    string            `json:"model"`
		Messages []recordedMessage `json:"messages"`
		Thinking struct {
			Type string `json:"type"`
		} `json:"thinking"`
		ReasoningEffort string `json:"reasoning_effort"`
	} `json:"request"`
	Response json.RawMessage `json:"response"`
}

type providerRecording struct {
	Version         int               `json:"version"`
	Provider        string            `json:"provider"`
	Model           string            `json:"requested_model"`
	Endpoint        string            `json:"endpoint"`
	SyntheticOnly   bool              `json:"synthetic_only"`
	InitialMessages []recordedMessage `json:"initial_messages"`
	Tools           []struct {
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
	Turns         []recordedTurn `json:"turns"`
	ThinkingProbe *struct {
		First     recordedTurn `json:"first"`
		Preserved recordedTurn `json:"preserved"`
		Omitted   recordedTurn `json:"omitted"`
	} `json:"thinking_probe"`
}

func loadProviderRecording(t *testing.T) *providerRecording {
	t.Helper()
	raw, err := os.ReadFile("testdata/provider_recordings/deepseek-flash.json")
	if err != nil {
		t.Fatal(err)
	}
	var recording providerRecording
	if err := json.Unmarshal(raw, &recording); err != nil {
		t.Fatal(err)
	}
	if recording.Version != 1 || recording.Provider != "deepseek" || !recording.SyntheticOnly || recording.Endpoint != "https://api.deepseek.com/v1/chat/completions" {
		t.Fatal("fixture provenance is not the authorized synthetic DeepSeek recording")
	}
	if len(recording.Turns) != 3 {
		t.Fatal("expected the recorded two-tool, single-tool, final-answer conversation")
	}
	return &recording
}

type recordedReplay struct {
	fixture *providerRecording
	client  contract.LLM
	mu      sync.Mutex
	invalid []error
	corrupt bool
}

func (model *recordedReplay) reject(err error) error {
	model.mu.Lock()
	defer model.mu.Unlock()
	model.invalid = append(model.invalid, err)
	return err
}

func (model *recordedReplay) invalidCount() int {
	model.mu.Lock()
	defer model.mu.Unlock()
	return len(model.invalid)
}

func (model *recordedReplay) Chat(ctx context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	if model.corrupt {
		for i, message := range request.Messages {
			if message.Role == "tool" {
				request.Messages = append(append([]contract.Message(nil), request.Messages[:i]...), request.Messages[i+1:]...)
				break
			}
		}
	}
	if err := strictOpenAIHistory(request.Messages); err != nil {
		return nil, model.reject(err)
	}
	return model.client.Chat(ctx, request)
}

func (*recordedReplay) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, errors.New("recorded non-streaming fixture does not stream")
}

func newRecordedReplay(t *testing.T, fixture *providerRecording) *recordedReplay {
	t.Helper()
	model := &recordedReplay{fixture: fixture}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request recordedTurn
		if err := json.NewDecoder(r.Body).Decode(&request.Request); err != nil {
			model.reject(err)
			return
		}
		if request.Request.Model != fixture.Model || request.Request.Thinking.Type != "disabled" || request.Request.ReasoningEffort != "none" {
			model.reject(errors.New("request changed the existing non-thinking tool profile"))
		}
		index := -1
		for i, turn := range fixture.Turns {
			if reflect.DeepEqual(conversation(request.Request.Messages), conversation(turn.Request.Messages)) {
				index = i
				break
			}
		}
		if index < 0 {
			model.reject(errors.New("wire history differs from the provider-accepted recording"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fixture.Turns[index].Status)
		_, _ = w.Write(fixture.Turns[index].Response)
	}))
	t.Cleanup(server.Close)
	model.client = openai.New("fixture-placeholder", openai.WithBaseURL(server.URL), openai.WithThinkingControl("enabled", true))
	return model
}

type recordedTools struct {
	fixture *providerRecording
	ledger  *effectLedger
}

func (tools *recordedTools) ListTools(context.Context) ([]contract.ToolDef, error) {
	var definitions []contract.ToolDef
	for _, tool := range tools.fixture.Tools {
		definitions = append(definitions, contract.ToolDef{Name: tool.Function.Name, Description: tool.Function.Description, InputSchema: tool.Function.Parameters, ReadOnly: true})
	}
	return definitions, nil
}

func (tools *recordedTools) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	for _, turn := range tools.fixture.Turns {
		for _, message := range turn.Request.Messages {
			if message.Role == "tool" && message.ToolCallID == call.ID {
				tools.ledger.executed[call.ID]++
				result := &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: message.Content}
				tools.ledger.results[call.ID] = result
				return result, nil
			}
		}
	}
	return nil, errors.New("tool call is absent from the real synthetic conversation")
}

func newRecordedRecovery(t *testing.T, fixture *providerRecording, controlled bool) (*recoveryRun, *recordedReplay) {
	t.Helper()
	model := newRecordedReplay(t, fixture)
	run := newRecoveryRun()
	run.recordedLLM = model
	run.recordedTools = &recordedTools{fixture: fixture, ledger: run.ledger}
	run.recordedInitial = conversation(fixture.InitialMessages)
	run.recordedOpts = &stdlib.ToolLoopOpts{Model: fixture.Model, MaxTokens: 512, MaxIterations: 6}
	if controlled {
		run.controlled, run.store = true, loom.NewMemStore()
		run.recordedOpts.MaxIterations = 1
		run.recordedOpts.Control = &stdlib.ToolLoopControl{ID: "recorded", InitialTotalRounds: 1}
	}
	return run, model
}

func requireRecordedEffects(t *testing.T, fixture *providerRecording, run *recoveryRun, model *recordedReplay) {
	t.Helper()
	ids := map[string]bool{}
	for _, turn := range fixture.Turns {
		for _, message := range turn.Request.Messages {
			if message.Role == "tool" {
				ids[message.ToolCallID] = true
			}
		}
	}
	for id := range ids {
		if run.ledger.executed[id] != 1 {
			t.Fatalf("recorded tool operation executed %d times, want 1", run.ledger.executed[id])
		}
	}
	if len(ids) != 3 || model.invalidCount() > 0 || run.journal.diverged != nil {
		t.Fatalf("recorded history, operation identity, or protocol diverged: invalid=%d", model.invalidCount())
	}
}

func TestRecoveryRecordedProviderEveryBoundary(t *testing.T) {
	fixture := loadProviderRecording(t)
	for _, controlled := range []bool{false, true} {
		base, model := newRecordedRecovery(t, fixture, controlled)
		want, err := base.attempt(t)
		if err != nil || want != "DONE:5" || len(base.journal.entries) != 6 {
			t.Fatalf("recorded baseline failed: output=%q operations=%d error=%v", want, len(base.journal.entries), err)
		}
		requireRecordedEffects(t, fixture, base, model)
		if controlled && base.lastPause == nil {
			t.Fatal("real recorded conversation did not exercise a saved round-budget pause")
		}
		for index := range base.journal.entries {
			for _, mode := range allCrashModes {
				t.Run(fmt.Sprintf("controlled=%v/op%d/%s", controlled, index, mode), func(t *testing.T) {
					run, model := newRecordedRecovery(t, fixture, controlled)
					run.journal.crashAt, run.journal.crashMode = index, mode
					if _, err := run.attempt(t); !errors.Is(err, errRecoveryCrash) {
						t.Fatalf("recorded run missed its crash boundary: %v", err)
					}
					if output := completeAfterCrash(t, run); output != want {
						t.Fatalf("recorded replay output differs: %q", output)
					}
					for _, entry := range run.journal.entries {
						if !entry.done {
							t.Fatal("recorded run left an unresolved journal entry")
						}
					}
					requireRecordedEffects(t, fixture, run, model)
				})
			}
		}
	}
}

func TestRecoveryRecordedUnknownOutcomeAndHistoryMutants(t *testing.T) {
	fixture := loadProviderRecording(t)
	run, model := newRecordedRecovery(t, fixture, false)
	run.journal.crashAt, run.journal.crashMode = 1, crashAfterEffect
	if _, err := run.attempt(t); !errors.Is(err, errRecoveryCrash) {
		t.Fatal(err)
	}
	before := make(map[string]int, len(run.ledger.executed))
	for id, count := range run.ledger.executed {
		before[id] = count
	}
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := run.attempt(t); !errors.Is(err, stdlib.ErrJournalOutcomeUnknown) {
			t.Fatalf("recorded unknown operation was not fenced: %v", err)
		}
		if !reflect.DeepEqual(run.ledger.executed, before) {
			t.Fatal("unknown recorded operation was blindly dispatched again")
		}
	}
	// An incorrect host resolution saying 'not performed' reruns a completed
	// pure tool; its counter catches the error without changing any HTTP status.
	run.journal.entries = run.journal.entries[:1]
	if _, err := run.attempt(t); err != nil {
		t.Fatal(err)
	}
	duplicated := false
	for _, count := range run.ledger.executed {
		duplicated = duplicated || count > 1
	}
	if !duplicated || model.invalidCount() != 0 {
		t.Fatal("the unknown-outcome mutation did not demonstrate the effect invariant")
	}
	broken, validator := newRecordedRecovery(t, fixture, false)
	validator.corrupt = true
	if _, err := broken.attempt(t); err == nil || validator.invalidCount() == 0 {
		t.Fatal("dropping a real tool result was not rejected by the independent transcript validator")
	}
}

func TestRecoveryRecordedThinkingObservationIsNotFabricated(t *testing.T) {
	fixture := loadProviderRecording(t)
	probe := fixture.ThinkingProbe
	if probe == nil || probe.First.Status != 200 || probe.Preserved.Status != 200 {
		t.Fatal("real thinking requests were not captured successfully")
	}
	var first struct {
		Choices []struct {
			Message recordedMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(probe.First.Response, &first); err != nil || len(first.Choices) != 1 {
		t.Fatal("invalid captured thinking envelope")
	}
	message := first.Choices[0].Message
	if message.ReasoningContent != "[provider continuation redacted]" || message.ReasoningBytes < 1 || len(message.ReasoningSHA256) != 64 || len(message.ToolCalls) < 2 {
		t.Fatal("thinking was not actually exercised or its private continuation was not redacted")
	}
	if probe.Omitted.Status != 200 {
		t.Fatal("this capture observed HTTP 200; never substitute an expected HTTP 400")
	}
	for _, message := range probe.Omitted.Request.Messages {
		if strings.TrimSpace(message.ReasoningContent) != "" {
			t.Fatal("the recorded omission request still carries continuation data")
		}
	}
}
