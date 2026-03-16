package loom_test

import (
	"context"
	"testing"

	"github.com/jinyitao123/loom"
)

func TestRouter_Always(t *testing.T) {
	r := loom.Always("next")
	next, err := r(context.Background(), loom.State{})
	assertNoError(t, err)
	if next != "next" {
		t.Errorf("got %q", next)
	}
}

func TestRouter_End(t *testing.T) {
	r := loom.End()
	next, err := r(context.Background(), loom.State{})
	assertNoError(t, err)
	if next != "" {
		t.Errorf("got %q, want empty", next)
	}
}

func TestRouter_Branch_MatchFound(t *testing.T) {
	r := loom.Branch("action", map[string]string{
		"approve": "step_a",
		"reject":  "step_b",
	}, "fallback")

	next, _ := r(context.Background(), loom.State{"action": "approve"})
	if next != "step_a" {
		t.Errorf("got %q", next)
	}
}

func TestRouter_Branch_Fallback(t *testing.T) {
	r := loom.Branch("action", map[string]string{
		"approve": "step_a",
	}, "fallback")

	next, _ := r(context.Background(), loom.State{"action": "unknown"})
	if next != "fallback" {
		t.Errorf("got %q", next)
	}
}

func TestRouter_Branch_MissingKey(t *testing.T) {
	r := loom.Branch("action", map[string]string{}, "fallback")

	next, _ := r(context.Background(), loom.State{})
	if next != "fallback" {
		t.Errorf("got %q", next)
	}
}

func TestRouter_Condition_True(t *testing.T) {
	r := loom.Condition(
		func(s loom.State) bool { return s["score"].(float64) >= 0.7 },
		"pass", "fail",
	)
	next, _ := r(context.Background(), loom.State{"score": 0.8})
	if next != "pass" {
		t.Errorf("got %q", next)
	}
}

func TestRouter_Condition_False(t *testing.T) {
	r := loom.Condition(
		func(s loom.State) bool { return s["score"].(float64) >= 0.7 },
		"pass", "fail",
	)
	next, _ := r(context.Background(), loom.State{"score": 0.3})
	if next != "fail" {
		t.Errorf("got %q", next)
	}
}

func TestRouter_Branch_IntValue(t *testing.T) {
	r := loom.Branch("code", map[string]string{"42": "found"}, "not_found")
	next, _ := r(context.Background(), loom.State{"code": 42})
	if next != "found" {
		t.Errorf("got %q", next)
	}
}

func TestRouter_BranchFunc(t *testing.T) {
	r := loom.BranchFunc(
		func(s loom.State) string {
			if v, ok := s["level"].(int); ok && v > 5 {
				return "high"
			}
			return "low"
		},
		map[string]string{"high": "step_a", "low": "step_b"},
		"step_c",
	)

	next, _ := r(context.Background(), loom.State{"level": 10})
	if next != "step_a" {
		t.Errorf("got %q", next)
	}

	next, _ = r(context.Background(), loom.State{"level": 2})
	if next != "step_b" {
		t.Errorf("got %q", next)
	}
}
