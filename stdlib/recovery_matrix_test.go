package stdlib_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

// This file is the W3 recovery regression: a host that replays a run from the
// start against a durable operation journal must never execute a tool effect
// twice, never send a provider a malformed history, and must reach the same
// final answer as an uninterrupted run, whichever operation boundary it
// crashed at. The journal below is deliberately independent of any host
// implementation; it only follows the ExecutionJournal contract.

var errRecoveryCrash = errors.New("simulated process crash")

// crashMode names the four distinguishable places a process can die around one
// journaled operation.
type crashMode string

const (
	// The process dies before anything about the operation was persisted.
	crashBeforeStart crashMode = "before_start_record"
	// The start was persisted but perform never ran.
	crashAfterStart crashMode = "after_start_before_effect"
	// The effect happened, the response was never persisted.
	crashAfterEffect crashMode = "after_effect_before_response"
	// The response was persisted; the process dies right after.
	crashAfterResponse crashMode = "after_response_record"
)

var allCrashModes = []crashMode{crashBeforeStart, crashAfterStart, crashAfterEffect, crashAfterResponse}

type journalEntry struct {
	segment  string
	kind     stdlib.OperationKind
	inputSum string
	done     bool
	response json.RawMessage
	callID   string // tool operations only
}

// sequenceJournal is a durable, index-addressed operation log. cursor is
// volatile and restarts with every attempt, like a host replaying from scratch.
type sequenceJournal struct {
	entries []journalEntry
	cursor  int    // position within the current segment
	segment string // a controlled loop journals each resumed slice separately

	crashAt   int // operation index to crash at; -1 for none
	crashMode crashMode
	spent     map[int]bool // operation indexes that already crashed once
	diverged  error
}

type recoveryCtxKey struct{}

func (*sequenceJournal) Active(ctx context.Context) bool {
	active, _ := ctx.Value(recoveryCtxKey{}).(bool)
	return active
}

func inputChecksum(input any) string {
	raw, _ := json.Marshal(input)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (journal *sequenceJournal) Execute(_ context.Context, operation stdlib.JournalOperation, perform stdlib.JournalPerform) (json.RawMessage, error) {
	position := journal.cursor
	journal.cursor++
	sum := inputChecksum(operation.Input)
	if found := journal.find(position); found >= 0 {
		entry := journal.entries[found]
		if entry.kind != operation.Kind || entry.inputSum != sum {
			journal.diverged = fmt.Errorf("replay diverged at %s operation %d: recorded %s, now %s", journal.segment, position, entry.kind, operation.Kind)
			return nil, journal.diverged
		}
		if !entry.done {
			return nil, fmt.Errorf("operation %d: %w", found, stdlib.ErrJournalOutcomeUnknown)
		}
		return entry.response, nil
	}
	index := len(journal.entries)
	fire := func(mode crashMode) bool {
		if journal.crashAt == index && journal.crashMode == mode && !journal.spent[index] {
			if journal.spent == nil {
				journal.spent = map[int]bool{}
			}
			journal.spent[index] = true
			return true
		}
		return false
	}
	if fire(crashBeforeStart) {
		return nil, errRecoveryCrash
	}
	entry := journalEntry{segment: journal.segment, kind: operation.Kind, inputSum: sum}
	if call, ok := operation.Input.(contract.ToolCall); ok {
		entry.callID = call.ID
	}
	journal.entries = append(journal.entries, entry)
	if fire(crashAfterStart) {
		return nil, errRecoveryCrash
	}
	response, err := perform()
	if err != nil {
		return nil, err
	}
	if fire(crashAfterEffect) {
		return nil, errRecoveryCrash
	}
	raw, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	journal.entries[index].done = true
	journal.entries[index].response = raw
	if fire(crashAfterResponse) {
		return nil, errRecoveryCrash
	}
	return raw, nil
}

// find returns the index of the position-th entry of the current segment, or -1.
func (journal *sequenceJournal) find(position int) int {
	seen := 0
	for index, entry := range journal.entries {
		if entry.segment != journal.segment {
			continue
		}
		if seen == position {
			return index
		}
		seen++
	}
	return -1
}

// beginSegment restarts the position counter, as a host does at every loop step.
func (journal *sequenceJournal) beginSegment(segment string) {
	journal.segment, journal.cursor = segment, 0
}

// reconcile is what a host does with an unresolved operation: a model call
// with no durable response is simply asked again (it has no external effect),
// a tool call is settled from the effect ledger if it ran, or dropped so it
// runs once if it never did.
func (journal *sequenceJournal) reconcile(ledger *effectLedger) (resolved int) {
	kept := journal.entries[:0]
	for _, entry := range journal.entries {
		if entry.done {
			kept = append(kept, entry)
			continue
		}
		resolved++
		if entry.kind == stdlib.OperationTool {
			if result, ran := ledger.results[entry.callID]; ran {
				entry.done = true
				entry.response, _ = json.Marshal(result)
				kept = append(kept, entry)
			}
			continue
		}
	}
	journal.entries = kept
	return resolved
}

// effectLedger is the outside world: it survives every crash and counts how
// many times each tool effect really happened.
type effectLedger struct {
	executed map[string]int
	results  map[string]*contract.ToolResult
}

func newEffectLedger() *effectLedger {
	return &effectLedger{executed: map[string]int{}, results: map[string]*contract.ToolResult{}}
}

type ledgerTools struct{ ledger *effectLedger }

func (*ledgerTools) ListTools(context.Context) ([]contract.ToolDef, error) {
	return []contract.ToolDef{
		{Name: "lookup", ReadOnly: true},
		{Name: "write"},
	}, nil
}

func (tools *ledgerTools) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	tools.ledger.executed[call.ID]++
	result := &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: fmt.Sprintf("%s#%d", call.Name, tools.ledger.executed[call.ID])}
	tools.ledger.results[call.ID] = result
	return result, nil
}

// historyValidator is a provider-format check written independently of the
// ToolLoop's own transcript validation.
type historyValidator func([]contract.Message) error

// strictOpenAIHistory encodes the rules OpenAI-compatible providers (including
// DeepSeek) enforce: every assistant tool call is answered by exactly one tool
// message before the next non-tool message, and tool messages answer nothing else.
func strictOpenAIHistory(messages []contract.Message) error {
	seen := map[string]bool{}
	open := map[string]bool{}
	for position, message := range messages {
		switch message.Role {
		case "system", "user":
			if len(open) > 0 {
				return fmt.Errorf("message %d: %s message while tool calls are unanswered", position, message.Role)
			}
			if len(message.ToolCalls) > 0 || message.ToolCallID != "" {
				return fmt.Errorf("message %d: %s message carries tool fields", position, message.Role)
			}
		case "assistant":
			if len(open) > 0 {
				return fmt.Errorf("message %d: assistant message while tool calls are unanswered", position)
			}
			if message.Content == "" && len(message.ToolCalls) == 0 {
				return fmt.Errorf("message %d: empty assistant message", position)
			}
			for _, call := range message.ToolCalls {
				if call.ID == "" || call.Name == "" || seen[call.ID] {
					return fmt.Errorf("message %d: invalid or duplicate tool call %q", position, call.ID)
				}
				if !json.Valid([]byte(call.Args)) {
					return fmt.Errorf("message %d: tool call %q has invalid JSON arguments", position, call.ID)
				}
				seen[call.ID] = true
				open[call.ID] = true
			}
		case "tool":
			if !open[message.ToolCallID] {
				return fmt.Errorf("message %d: tool result %q answers no open call", position, message.ToolCallID)
			}
			delete(open, message.ToolCallID)
		default:
			return fmt.Errorf("message %d: unknown role %q", position, message.Role)
		}
	}
	if len(open) > 0 {
		return fmt.Errorf("history ends with %d unanswered tool calls", len(open))
	}
	if len(messages) == 0 || messages[len(messages)-1].Role == "assistant" && len(messages[len(messages)-1].ToolCalls) == 0 {
		return errors.New("request must end with a user or tool message")
	}
	return nil
}

// scriptedModel answers from the shape of the history alone, so a replay that
// rebuilds the same history gets the same answer, like a deterministic model.
type scriptedModel struct {
	validate historyValidator
	requests int
	invalid  []error
}

func (*scriptedModel) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, errors.New("streaming is not used")
}

func (model *scriptedModel) Chat(_ context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	model.requests++
	if err := model.validate(request.Messages); err != nil {
		model.invalid = append(model.invalid, err)
		return nil, fmt.Errorf("provider rejected the history: %w", err)
	}
	assistantTurns := 0
	var last string
	for _, message := range request.Messages {
		if message.Role == "assistant" {
			assistantTurns++
		}
		if message.Role == "tool" {
			last += message.ToolCallID + "=" + message.Content + ";"
		}
	}
	switch assistantTurns {
	case 0:
		return &contract.ChatResponse{ToolCalls: []contract.ToolCall{
			{ID: "c1", Name: "lookup", Args: `{"q":"a"}`},
			{ID: "c2", Name: "write", Args: `{"v":1}`},
		}, StopReason: "tool_calls"}, nil
	case 1:
		return &contract.ChatResponse{ToolCalls: []contract.ToolCall{{ID: "c3", Name: "write", Args: `{"v":2}`}}, StopReason: "tool_calls"}, nil
	default:
		return &contract.ChatResponse{Content: "done:" + last, StopReason: "stop"}, nil
	}
}

type recoveryRun struct {
	journal *sequenceJournal
	ledger  *effectLedger
	model   *scriptedModel
	// Recorded-provider runs use the same crash journal and checkpoint harness.
	recordedLLM     contract.LLM
	recordedTools   contract.ToolDispatcher
	recordedInitial []contract.Message
	recordedOpts    *stdlib.ToolLoopOpts

	// controlled runs the loop in durable model-round slices: the host's
	// checkpoint store and the last pause survive a crash, like its database.
	controlled bool
	store      loom.Store
	lastPause  *loom.RunResult
}

func (run *recoveryRun) adapters() (contract.LLM, contract.ToolDispatcher) {
	if run.recordedLLM != nil {
		return run.recordedLLM, run.recordedTools
	}
	return run.model, &ledgerTools{ledger: run.ledger}
}

func (run *recoveryRun) initialMessages() []contract.Message {
	if run.recordedInitial != nil {
		return append([]contract.Message(nil), run.recordedInitial...)
	}
	return []contract.Message{{Role: "user", Content: "do the work"}}
}

func newRecoveryRun() *recoveryRun {
	return &recoveryRun{
		journal: &sequenceJournal{crashAt: -1},
		ledger:  newEffectLedger(),
		model:   &scriptedModel{validate: strictOpenAIHistory},
	}
}

// attempt replays the whole step from the start against the durable journal.
func (run *recoveryRun) attempt(t *testing.T) (string, error) {
	t.Helper()
	if run.controlled {
		return run.attemptControlled(t)
	}
	run.journal.cursor = 0
	innerLLM, innerTools := run.adapters()
	llm := stdlib.NewJournaledLLM(innerLLM, run.journal)
	tools := stdlib.NewJournaledToolDispatcher(innerTools, run.journal, stdlib.JournaledToolOpts{SerializeWhenActive: true})
	opts := stdlib.ToolLoopOpts{Model: "m", MaxIterations: 6}
	if run.recordedOpts != nil {
		opts = *run.recordedOpts
	}
	step := stdlib.NewToolLoopStep(llm, tools, opts)
	ctx := context.WithValue(context.Background(), recoveryCtxKey{}, true)
	delta, err := step(ctx, loom.State{"messages": run.initialMessages()})
	if err != nil {
		return "", err
	}
	output, _ := delta["output"].(string)
	return output, nil
}

func referenceRun(t *testing.T) (output string, operations int) {
	t.Helper()
	run := newRecoveryRun()
	output, err := run.attempt(t)
	if err != nil {
		t.Fatalf("uninterrupted run failed: %v", err)
	}
	if !strings.HasPrefix(output, "done:") {
		t.Fatalf("uninterrupted output = %q", output)
	}
	if len(run.model.invalid) > 0 {
		t.Fatalf("uninterrupted run produced an invalid history: %v", run.model.invalid)
	}
	return output, len(run.journal.entries)
}

func requireExactlyOnceEffects(t *testing.T, ledger *effectLedger) {
	t.Helper()
	for _, id := range []string{"c1", "c2", "c3"} {
		if ledger.executed[id] != 1 {
			t.Fatalf("tool call %s executed %d times, want exactly 1 (ledger %v)", id, ledger.executed[id], ledger.executed)
		}
	}
}

// completeAfterCrash drives recovery the way a host does: replay; if the
// journal reports an unresolved operation, reconcile it and replay again.
func completeAfterCrash(t *testing.T, run *recoveryRun) string {
	t.Helper()
	for attempts := 0; attempts < 8; attempts++ {
		output, err := run.attempt(t)
		switch {
		case err == nil:
			return output
		case errors.Is(err, stdlib.ErrJournalOutcomeUnknown):
			if run.journal.reconcile(run.ledger) == 0 {
				t.Fatalf("outcome unknown but nothing to reconcile: %v", err)
			}
		default:
			t.Fatalf("recovery attempt %d failed: %v", attempts+1, err)
		}
	}
	t.Fatal("recovery did not converge")
	return ""
}

func TestRecoveryMatrixEveryOperationBoundary(t *testing.T) {
	want, operations := referenceRun(t)
	if operations != 6 {
		t.Fatalf("script changed: %d journaled operations, want 6", operations)
	}
	for index := 0; index < operations; index++ {
		for _, mode := range allCrashModes {
			t.Run(fmt.Sprintf("op%d/%s", index, mode), func(t *testing.T) {
				run := newRecoveryRun()
				run.journal.crashAt, run.journal.crashMode = index, mode
				if _, err := run.attempt(t); !errors.Is(err, errRecoveryCrash) {
					t.Fatalf("first attempt did not stop at the crash: %v", err)
				}
				got := completeAfterCrash(t, run)
				if got != want {
					t.Fatalf("recovered output %q differs from uninterrupted %q", got, want)
				}
				requireExactlyOnceEffects(t, run.ledger)
				if len(run.model.invalid) > 0 {
					t.Fatalf("a request had an invalid history: %v", run.model.invalid)
				}
				if run.journal.diverged != nil {
					t.Fatal(run.journal.diverged)
				}
			})
		}
	}
}

// A crash at the effect boundary must be reported as unknown, never retried
// silently: replaying the journal alone may not execute the tool again.
func TestRecoveryUnknownOutcomeIsNeverRetriedBlindly(t *testing.T) {
	run := newRecoveryRun()
	run.journal.crashAt, run.journal.crashMode = 2, crashAfterEffect // operation 2 is c2, the first write
	if _, err := run.attempt(t); !errors.Is(err, errRecoveryCrash) {
		t.Fatalf("first attempt: %v", err)
	}
	if run.ledger.executed["c2"] != 1 {
		t.Fatalf("effect did not happen before the crash: %v", run.ledger.executed)
	}
	for replay := 0; replay < 3; replay++ {
		if _, err := run.attempt(t); !errors.Is(err, stdlib.ErrJournalOutcomeUnknown) {
			t.Fatalf("replay %d without reconciliation: %v", replay, err)
		}
		if run.ledger.executed["c2"] != 1 {
			t.Fatalf("replay %d re-executed the unresolved tool: %v", replay, run.ledger.executed)
		}
	}
}

// Every attempt crashes at the next new operation until the run finishes: a
// host that keeps dying must still converge without duplicating effects.
func TestRecoveryRepeatedCrashesConverge(t *testing.T) {
	want, operations := referenceRun(t)
	for _, mode := range allCrashModes {
		t.Run(string(mode), func(t *testing.T) {
			run := newRecoveryRun()
			var got string
			for attempts := 0; attempts <= operations*3; attempts++ {
				run.journal.crashAt, run.journal.crashMode = len(run.journal.entries), mode
				output, err := run.attempt(t)
				if err == nil {
					got = output
					break
				}
				if errors.Is(err, stdlib.ErrJournalOutcomeUnknown) {
					run.journal.reconcile(run.ledger)
					continue
				}
				if !errors.Is(err, errRecoveryCrash) {
					t.Fatalf("attempt %d: %v", attempts, err)
				}
			}
			if got != want {
				t.Fatalf("converged output %q, want %q", got, want)
			}
			requireExactlyOnceEffects(t, run.ledger)
			if len(run.model.invalid) > 0 {
				t.Fatalf("invalid history during recovery: %v", run.model.invalid)
			}
		})
	}
}

// The validator itself must reject the shapes providers reject, otherwise the
// matrix above proves nothing.
func TestRecoveryHistoryValidatorRejectsBrokenTranscripts(t *testing.T) {
	call := contract.ToolCall{ID: "x", Name: "write", Args: `{}`}
	broken := map[string][]contract.Message{
		"unanswered call":    {{Role: "user", Content: "u"}, {Role: "assistant", ToolCalls: []contract.ToolCall{call}}},
		"orphan tool result": {{Role: "user", Content: "u"}, {Role: "tool", ToolCallID: "x", Content: "r"}},
		"user before result": {{Role: "user", Content: "u"}, {Role: "assistant", ToolCalls: []contract.ToolCall{call}}, {Role: "user", Content: "again"}},
		"duplicate call id":  {{Role: "user", Content: "u"}, {Role: "assistant", ToolCalls: []contract.ToolCall{call}}, {Role: "tool", ToolCallID: "x", Content: "r"}, {Role: "assistant", ToolCalls: []contract.ToolCall{call}}, {Role: "tool", ToolCallID: "x", Content: "r"}},
		"bad arguments":      {{Role: "user", Content: "u"}, {Role: "assistant", ToolCalls: []contract.ToolCall{{ID: "y", Name: "w", Args: `{`}}}, {Role: "tool", ToolCallID: "y", Content: "r"}},
		"ends on assistant":  {{Role: "user", Content: "u"}, {Role: "assistant", Content: "hello"}},
	}
	for name, messages := range broken {
		if err := strictOpenAIHistory(messages); err == nil {
			t.Fatalf("%s: validator accepted a broken transcript", name)
		}
	}
	ok := []contract.Message{{Role: "user", Content: "u"}, {Role: "assistant", ToolCalls: []contract.ToolCall{call}}, {Role: "tool", ToolCallID: "x", Content: "r"}}
	if err := strictOpenAIHistory(ok); err != nil {
		t.Fatalf("validator rejected a valid transcript: %v", err)
	}
}

// attemptControlled drives a sliced loop the way a host does: run until the
// loop pauses at its round limit, grant more rounds, and resume from the
// pause. A crash inside a slice leaves the last pause in place, so recovery
// resumes that same pause and the slice's journal segment is replayed.
func (run *recoveryRun) attemptControlled(t *testing.T) (string, error) {
	t.Helper()
	run.journal.beginSegment("")
	innerLLM, innerTools := run.adapters()
	llm := stdlib.NewJournaledLLM(innerLLM, run.journal)
	tools := stdlib.NewJournaledToolDispatcher(innerTools, run.journal, stdlib.JournaledToolOpts{SerializeWhenActive: true})
	graph := loom.NewGraph("recovery-controlled", "chat", loom.WithCheckpointPolicy(loom.CheckpointRequired))
	graph.SetHooks(loom.HookPoints{Before: []loom.StepHook{func(_ context.Context, _ string, state loom.State) error {
		// Failed attempts may append an error checkpoint. Journal identity
		// belongs to the frozen slice entry, not that newer error sequence.
		segment := "entry-0"
		if run.lastPause != nil {
			segment = fmt.Sprint(run.lastPause.State["__checkpoint_seq"])
		}
		run.journal.beginSegment(segment)
		return nil
	}}})
	opts := stdlib.ToolLoopOpts{
		Model: "m", MaxIterations: 2,
		Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 2},
	}
	if run.recordedOpts != nil {
		opts = *run.recordedOpts
	}
	graph.AddStep("chat", stdlib.NewToolLoopStep(llm, tools, opts), loom.End())
	ctx := context.WithValue(context.Background(), recoveryCtxKey{}, true)
	for slices := 0; slices < 4; slices++ {
		var result *loom.RunResult
		var err error
		if run.lastPause == nil {
			result, err = graph.Run(ctx, loom.State{"__run_id": "run-1", "messages": run.initialMessages()}, run.store)
		} else {
			outcome, _, readErr := stdlib.ReadToolLoopOutcome(run.lastPause.State)
			if readErr != nil {
				return "", readErr
			}
			seq, _ := run.lastPause.State["__checkpoint_seq"].(int64)
			grant := stdlib.ToolLoopResumeGrant{ID: fmt.Sprintf("grant-%d", slices), ExpectedRunID: run.lastPause.RunID,
				ExpectedCheckpointSeq: seq, ExpectedYieldToken: run.lastPause.State["__yield_token"].(string),
				ExpectedSlice: outcome.Slice, AuthorizedTotalRounds: outcome.AuthorizedTotalRounds + 1}
			input, prepareErr := stdlib.PrepareToolLoopResume(run.lastPause.State, grant)
			if prepareErr != nil {
				return "", prepareErr
			}
			result, err = graph.Resume(ctx, run.lastPause.RunID, input, run.store)
		}
		if err != nil {
			return "", err
		}
		if result.Yielded {
			run.lastPause = result
			continue
		}
		output, _ := result.State["output"].(string)
		return output, nil
	}
	return "", errors.New("controlled loop did not finish within four slices")
}

func newControlledRecoveryRun() *recoveryRun {
	run := newRecoveryRun()
	run.controlled, run.store = true, loom.NewMemStore()
	return run
}

func TestRecoveryControlledLoopEveryOperationBoundary(t *testing.T) {
	reference := newControlledRecoveryRun()
	want, err := reference.attempt(t)
	if err != nil || !strings.HasPrefix(want, "done:") {
		t.Fatalf("uninterrupted controlled run: output %q err %v", want, err)
	}
	if segments := map[string]bool{}; true {
		for _, entry := range reference.journal.entries {
			segments[entry.segment] = true
		}
		if len(segments) != 2 {
			t.Fatalf("the controlled run used %d journal segments, want 2 (one per slice): %v", len(segments), segments)
		}
	}
	operations := len(reference.journal.entries)
	for index := 0; index < operations; index++ {
		for _, mode := range allCrashModes {
			t.Run(fmt.Sprintf("op%d/%s", index, mode), func(t *testing.T) {
				run := newControlledRecoveryRun()
				run.journal.crashAt, run.journal.crashMode = index, mode
				if _, err := run.attempt(t); !errors.Is(err, errRecoveryCrash) {
					t.Fatalf("first attempt did not stop at the crash: %v", err)
				}
				got := completeAfterCrash(t, run)
				if got != want {
					t.Fatalf("recovered output %q differs from uninterrupted %q", got, want)
				}
				requireExactlyOnceEffects(t, run.ledger)
				if len(run.model.invalid) > 0 {
					t.Fatalf("a request had an invalid history: %v", run.model.invalid)
				}
				if run.journal.diverged != nil {
					t.Fatal(run.journal.diverged)
				}
			})
		}
	}
}
