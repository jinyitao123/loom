package openai

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jinyitao123/loom/contract"
)

// ErrStageTimeout matches every StageTimeoutError.
var ErrStageTimeout = errors.New("openai: wait budget exceeded")

// StageTimeoutError reports a streamed call that exceeded a WaitBudget stage
// before the caller's own context ended. Stage is "first_chunk" or
// "stream_idle".
// 中文：流式调用在调用方 context 结束前超出了某个等待阶段的预算。
type StageTimeoutError struct {
	Stage  string
	Budget time.Duration
}

func (e *StageTimeoutError) Error() string {
	return "openai: " + e.Stage + " wait budget of " + e.Budget.String() + " exceeded"
}

func (e *StageTimeoutError) Unwrap() error { return ErrStageTimeout }

// waitWatch cancels a streamed request when the next model output is late:
// first by the first-chunk budget, then by the idle budget after each output.
type waitWatch struct {
	mu     sync.Mutex
	budget *contract.WaitBudget
	cancel context.CancelCauseFunc
	timer  *time.Timer
	done   bool
}

func newWaitWatch(budget *contract.WaitBudget, cancel context.CancelCauseFunc) *waitWatch {
	watch := &waitWatch{budget: budget, cancel: cancel}
	if budget != nil && budget.FirstChunkMS > 0 {
		watch.arm("first_chunk", time.Duration(budget.FirstChunkMS)*time.Millisecond)
	}
	return watch
}

func (w *waitWatch) arm(stage string, budget time.Duration) {
	w.timer = time.AfterFunc(budget, func() { w.cancel(&StageTimeoutError{Stage: stage, Budget: budget}) })
}

// progress records model output: the first-chunk bound is met and the idle
// bound starts over.
func (w *waitWatch) progress() {
	if w.budget == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return
	}
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	if w.budget.IdleMS > 0 {
		w.arm("stream_idle", time.Duration(w.budget.IdleMS)*time.Millisecond)
	}
}

func (w *waitWatch) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done = true
	if w.timer != nil {
		w.timer.Stop()
	}
}

// stageTimeout returns the wait budget a request context was cancelled for,
// or nil when it was not cancelled by one.
func stageTimeout(ctx context.Context) *StageTimeoutError {
	var stage *StageTimeoutError
	if errors.As(context.Cause(ctx), &stage) {
		return stage
	}
	return nil
}
