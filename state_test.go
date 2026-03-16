package loom_test

import (
	"testing"

	"github.com/jinyitao123/loom"
)

func TestState_Merge_Overwrite(t *testing.T) {
	cfg := loom.NewMergeConfig()
	s := loom.State{"a": "old", "b": "keep"}
	updated := s.Merge(loom.State{"a": "new", "c": "added"}, cfg)

	if updated["a"] != "new" {
		t.Errorf("a = %v, want new", updated["a"])
	}
	if updated["b"] != "keep" {
		t.Errorf("b = %v, want keep", updated["b"])
	}
	if updated["c"] != "added" {
		t.Errorf("c = %v, want added", updated["c"])
	}
}

func TestState_Merge_AppendSlice(t *testing.T) {
	cfg := loom.NewMergeConfig()
	cfg.Register("messages", loom.AppendSlice)

	s := loom.State{"messages": []any{"hello"}}
	updated := s.Merge(loom.State{"messages": []any{"world"}}, cfg)

	msgs := updated["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want 2", len(msgs))
	}
	if msgs[0] != "hello" || msgs[1] != "world" {
		t.Errorf("messages = %v, want [hello, world]", msgs)
	}
}

func TestState_Merge_SumInt(t *testing.T) {
	cfg := loom.NewMergeConfig()
	cfg.Register("count", loom.SumInt)

	s := loom.State{"count": 10}
	updated := s.Merge(loom.State{"count": 5}, cfg)

	if updated["count"] != 15 {
		t.Errorf("count = %v, want 15", updated["count"])
	}
}

func TestState_Merge_SumFloat(t *testing.T) {
	cfg := loom.NewMergeConfig()
	cfg.Register("score", loom.SumFloat)

	s := loom.State{"score": 0.7}
	updated := s.Merge(loom.State{"score": 0.3}, cfg)

	score := updated["score"].(float64)
	if score < 0.99 || score > 1.01 {
		t.Errorf("score = %v, want ~1.0", score)
	}
}

func TestState_Merge_NilExisting(t *testing.T) {
	cfg := loom.NewMergeConfig()
	cfg.Register("messages", loom.AppendSlice)

	s := loom.State{}
	updated := s.Merge(loom.State{"messages": []any{"first"}}, cfg)

	msgs := updated["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want 1", len(msgs))
	}
}

func TestState_Merge_NilConfig(t *testing.T) {
	s := loom.State{"a": "old"}
	updated := s.Merge(loom.State{"a": "new"}, nil)

	if updated["a"] != "new" {
		t.Errorf("a = %v, want new", updated["a"])
	}
}

func TestState_Merge_CustomPolicy(t *testing.T) {
	maxInt := loom.MergePolicy(func(existing, incoming any) any {
		e, _ := existing.(int)
		i, _ := incoming.(int)
		if i > e {
			return i
		}
		return e
	})

	cfg := loom.NewMergeConfig()
	cfg.Register("high_score", maxInt)

	s := loom.State{"high_score": 100}
	s = s.Merge(loom.State{"high_score": 80}, cfg)
	if s["high_score"] != 100 {
		t.Errorf("should keep 100, got %v", s["high_score"])
	}

	s = s.Merge(loom.State{"high_score": 150}, cfg)
	if s["high_score"] != 150 {
		t.Errorf("should update to 150, got %v", s["high_score"])
	}
}

func TestState_Merge_MultipleAppendSlice(t *testing.T) {
	cfg := loom.NewMergeConfig()
	cfg.Register("messages", loom.AppendSlice)
	cfg.Register("tool_results", loom.AppendSlice)

	s := loom.State{
		"messages":     []any{"msg1"},
		"tool_results": []any{"result1"},
	}
	s = s.Merge(loom.State{
		"messages":     []any{"msg2"},
		"tool_results": []any{"result2"},
		"output":       "done",
	}, cfg)

	msgs := s["messages"].([]any)
	tools := s["tool_results"].([]any)
	if len(msgs) != 2 {
		t.Errorf("messages len = %d", len(msgs))
	}
	if len(tools) != 2 {
		t.Errorf("tool_results len = %d", len(tools))
	}
	if s["output"] != "done" {
		t.Errorf("output = %v", s["output"])
	}
}

func TestState_Merge_DoesNotMutateOriginal(t *testing.T) {
	cfg := loom.NewMergeConfig()
	original := loom.State{"a": "1", "b": "2"}
	_ = original.Merge(loom.State{"a": "changed"}, cfg)

	if original["a"] != "1" {
		t.Errorf("original mutated: a = %v", original["a"])
	}
}

func TestState_Marshal_Roundtrip(t *testing.T) {
	s := loom.State{"name": "test", "count": float64(42)}
	data, err := s.Marshal()
	assertNoError(t, err)

	restored, err := loom.UnmarshalState(data)
	assertNoError(t, err)

	if restored["name"] != "test" {
		t.Errorf("name = %v", restored["name"])
	}
	if restored["count"] != float64(42) {
		t.Errorf("count = %v", restored["count"])
	}
}
