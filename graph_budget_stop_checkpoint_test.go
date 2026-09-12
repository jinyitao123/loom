package loom

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestBudgetStopCheckpointResume(t *testing.T) {
	for _, mode := range []string{"resume", "resume_at"} {
		for _, replenish := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/replenish_%t", mode, replenish), func(t *testing.T) {
				ctx := context.Background()
				store := NewMemStore()
				g, executions := newBudgetStopCheckpointGraph(WithCheckpointPolicy(CheckpointRequired))
				first, err := g.Run(ctx, State{}, store)
				assertBudgetStop(t, first, err)
				if *executions != [2]int{1, 0} {
					t.Fatalf("initial executions = %v, want [1 0]", *executions)
				}
				assertBudgetStopTrace(t, first.State, "a")
				assertBudgetRemaining(t, first.State, 0)
				if first.LastStep != "b" || first.Yielded {
					t.Errorf("Run position = %q, yielded %t, want b without yield", first.LastStep, first.Yielded)
				}

				cp := readBudgetCheckpoint(t, store, g.Name, first.RunID)
				if cp.LastStep != "b" || cp.YieldPhase != "mid_step" || cp.Seq != 2 {
					t.Errorf("budget checkpoint = step %q phase %q seq %d, want b/mid_step/2", cp.LastStep, cp.YieldPhase, cp.Seq)
				}
				assertBudgetStopTrace(t, cp.State, "a")
				assertBudgetRemaining(t, cp.State, 0)
				historyKey := fmt.Sprintf("%s/%012d", first.RunID, cp.Seq)
				latestBefore, err := store.Get(ctx, "checkpoint:"+g.Name, first.RunID)
				if err != nil {
					t.Fatal(err)
				}
				historyBefore, err := store.Get(ctx, "checkpoint:"+g.Name, historyKey)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(latestBefore, historyBefore) {
					t.Error("budget history differs from latest checkpoint")
				}

				input := State{}
				if replenish {
					input[budgetRemainingStateKey] = int64(1)
				}
				var resumed *RunResult
				if mode == "resume" {
					resumed, err = g.Resume(ctx, first.RunID, input, store)
				} else {
					resumed, err = g.ResumeAt(ctx, first.RunID, cp.Seq, input, store)
				}
				if resumed == nil {
					t.Fatalf("restore result is nil: %v", err)
				}
				if replenish {
					if err != nil || resumed.StopReason != StopCompleted {
						t.Errorf("replenished restore = stop %q error %v, want completed", resumed.StopReason, err)
					}
					if *executions != [2]int{1, 1} {
						t.Errorf("restored executions = %v, want [1 1] without repeating a", *executions)
					}
					assertBudgetStopTrace(t, resumed.State, "a", "b")
				} else {
					assertBudgetStop(t, resumed, err)
					if *executions != [2]int{1, 0} {
						t.Errorf("unfunded restore executions = %v, want [1 0]", *executions)
					}
					assertBudgetStopTrace(t, resumed.State, "a")
				}
				if resumed.LastStep != "b" {
					t.Errorf("restored last step = %q, want b", resumed.LastStep)
				}
				assertBudgetRemaining(t, resumed.State, 0)
				if mode == "resume" {
					if resumed.RunID != first.RunID {
						t.Errorf("Resume changed run ID from %q to %q", first.RunID, resumed.RunID)
					}
				} else {
					if resumed.RunID == first.RunID {
						t.Error("ResumeAt reused source run ID")
					}
					latestAfter, err := store.Get(ctx, "checkpoint:"+g.Name, first.RunID)
					if err != nil {
						t.Fatal(err)
					}
					historyAfter, err := store.Get(ctx, "checkpoint:"+g.Name, historyKey)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(latestBefore, latestAfter) || !bytes.Equal(historyBefore, historyAfter) {
						t.Error("ResumeAt modified a source checkpoint")
					}
				}
			})
		}
	}
}

func TestBudgetStopCheckpointWriteFailure(t *testing.T) {
	for _, required := range []bool{false, true} {
		t.Run(fmt.Sprintf("required_%t", required), func(t *testing.T) {
			var opts []GraphOption
			if required {
				opts = append(opts, WithCheckpointPolicy(CheckpointRequired))
			}
			g, executions := newBudgetStopCheckpointGraph(opts...)
			saveErr := errors.New("budget checkpoint write failed")
			store := &budgetStopCheckpointFailureStore{
				Store: NewMemStore(), namespace: "checkpoint:" + g.Name,
				latestKey: "budget-stop-write-failure", failure: saveErr,
			}
			terminal := &budgetStopCheckpointTerminalizer{}
			result, err := g.RunWithLifecycle(context.Background(), State{"__run_id": store.latestKey}, store, LifecycleHooks{
				Terminalizer: terminal,
			})
			if result == nil {
				t.Fatalf("Run result is nil: %v", err)
			}
			wantStop := StopBudget
			if required {
				wantStop = StopError
			}
			if result.StopReason != wantStop || !errors.Is(err, ErrBudgetExhausted) {
				t.Errorf("Run = stop %q error %v, want %q with ErrBudgetExhausted", result.StopReason, err, wantStop)
			}
			if errors.Is(err, saveErr) != required {
				t.Errorf("error wraps storage failure = %t, want %t: %v", errors.Is(err, saveErr), required, err)
			}
			if result.LastStep != "b" || *executions != [2]int{1, 0} {
				t.Errorf("Run position = %q executions %v, want b/[1 0]", result.LastStep, *executions)
			}
			assertBudgetStopTrace(t, result.State, "a")
			assertBudgetRemaining(t, result.State, 0)
			if store.latestAttempts != 2 {
				t.Errorf("latest Put attempts = %d, want successful a then failed b", store.latestAttempts)
			}

			latest := readBudgetCheckpoint(t, store, g.Name, result.RunID)
			if latest.LastStep != "a" || latest.YieldPhase != "" || latest.Seq != 1 {
				t.Errorf("durable latest = step %q phase %q seq %d, want unchanged a/empty/1", latest.LastStep, latest.YieldPhase, latest.Seq)
			}
			assertBudgetStopTrace(t, latest.State, "a")
			assertBudgetRemaining(t, latest.State, 0)
			history, err := g.History(context.Background(), store, result.RunID)
			if err != nil || len(history) != 1 || history[0].Seq != 1 {
				t.Errorf("history = %#v, error %v, want only saved a checkpoint", history, err)
			}
			if len(terminal.events) != 1 {
				t.Fatalf("terminal events = %d, want 1", len(terminal.events))
			}
			event := terminal.events[0]
			if event.StopReason != wantStop || event.LatestCheckpointSeq != 2 || event.LatestCheckpointPersisted {
				t.Errorf("terminal = stop %q seq %d persisted %t, want %q/2/false", event.StopReason, event.LatestCheckpointSeq, event.LatestCheckpointPersisted, wantStop)
			}
		})
	}
}

func TestBudgetStopCheckpointCompatibility(t *testing.T) {
	t.Run("nil_store", func(t *testing.T) {
		g, executions := newBudgetStopCheckpointGraph(WithCheckpointPolicy(CheckpointRequired))
		result, err := g.Run(context.Background(), State{}, nil)
		assertBudgetStop(t, result, err)
		if result.LastStep != "b" || *executions != [2]int{1, 0} {
			t.Errorf("Run position = %q executions %v, want b/[1 0]", result.LastStep, *executions)
		}
		assertBudgetStopTrace(t, result.State, "a")
		if _, ok := result.State["__seq"]; ok {
			t.Error("nil Store created a checkpoint sequence")
		}
	})
	t.Run("ordinary_error_discards_delta", func(t *testing.T) {
		store := NewMemStore()
		g := NewGraph("budget-stop-ordinary-error", "a", WithStepBudget(1), WithCheckpointPolicy(CheckpointRequired))
		stepErr := errors.New("ordinary step failure")
		g.AddStep("a", func(context.Context, State) (State, error) {
			return State{"partial": "must not merge"}, stepErr
		}, End())
		result, err := g.Run(context.Background(), State{}, store)
		if result == nil || result.StopReason != StopError || !errors.Is(err, stepErr) || errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("Run = %#v, error %v, want ordinary StopError", result, err)
		}
		cp := readBudgetCheckpoint(t, store, g.Name, result.RunID)
		for _, state := range []State{result.State, cp.State} {
			if _, ok := state["partial"]; ok {
				t.Error("ordinary step error merged its partial delta")
			}
			if state["__error"] != stepErr.Error() || state["__failed_step"] != "a" {
				t.Errorf("ordinary error metadata = %#v", state)
			}
			assertBudgetRemaining(t, state, 0)
		}
		if cp.LastStep != "a" || cp.YieldPhase != "" || cp.Seq != 1 {
			t.Errorf("ordinary error checkpoint = step %q phase %q seq %d, want a/empty/1", cp.LastStep, cp.YieldPhase, cp.Seq)
		}
	})
}

func newBudgetStopCheckpointGraph(opts ...GraphOption) (*Graph, *[2]int) {
	cfg := NewMergeConfig()
	cfg.Register("trace", AppendSlice)
	baseOpts := []GraphOption{WithStepBudget(1), WithCheckpointHistory(-1), WithMergeConfig(cfg)}
	g := NewGraph("budget-stop-checkpoint", "a", append(baseOpts, opts...)...)
	executions := &[2]int{}
	g.AddStep("a", func(context.Context, State) (State, error) {
		executions[0]++
		return State{"trace": []any{"a"}}, nil
	}, Always("b"))
	g.AddStep("b", func(context.Context, State) (State, error) {
		executions[1]++
		return State{"trace": []any{"b"}}, nil
	}, End())
	return g, executions
}

func assertBudgetStopTrace(t *testing.T, state State, want ...string) {
	t.Helper()
	expected := make([]any, len(want))
	for i, value := range want {
		expected[i] = value
	}
	if !reflect.DeepEqual(state["trace"], expected) {
		t.Errorf("trace = %#v, want %#v", state["trace"], expected)
	}
}

type budgetStopCheckpointFailureStore struct {
	Store
	namespace      string
	latestKey      string
	failure        error
	latestAttempts int
}

func (s *budgetStopCheckpointFailureStore) Put(ctx context.Context, ns, key string, value []byte) error {
	if ns == s.namespace && key == s.latestKey {
		s.latestAttempts++
		if s.latestAttempts == 2 {
			return s.failure
		}
	}
	return s.Store.Put(ctx, ns, key, value)
}

type budgetStopCheckpointTerminalizer struct {
	events []TerminalizationEvent
}

func (t *budgetStopCheckpointTerminalizer) Terminalize(_ context.Context, event TerminalizationEvent) error {
	t.events = append(t.events, event)
	return nil
}
