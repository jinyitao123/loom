package loom_test

import (
	"context"
	"sync"
	"testing"

	"github.com/jinyitao123/loom"
)

func BenchmarkGraph_LinearChain_3Steps(b *testing.B) {
	g := loom.NewGraph("bench", "a")
	g.AddStep("a", echoStep("x", "1"), loom.Always("b"))
	g.AddStep("b", echoStep("y", "2"), loom.Always("c"))
	g.AddStep("c", echoStep("z", "3"), loom.End())

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.Run(context.Background(), loom.State{}, nil)
	}
}

func BenchmarkGraph_LinearChain_3Steps_WithStore(b *testing.B) {
	store := loom.NewMemStore()
	g := loom.NewGraph("bench", "a")
	g.AddStep("a", echoStep("x", "1"), loom.Always("b"))
	g.AddStep("b", echoStep("y", "2"), loom.Always("c"))
	g.AddStep("c", echoStep("z", "3"), loom.End())

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.Run(context.Background(), loom.State{}, store)
	}
}

func BenchmarkGraph_Loop_10Iterations(b *testing.B) {
	g := loom.NewGraph("bench", "inc")
	g.AddStep("inc", counterStep("n"), loom.Condition(
		func(s loom.State) bool { n, _ := s["n"].(int); return n >= 10 },
		"", "inc",
	))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.Run(context.Background(), loom.State{"n": 0}, nil)
	}
}

func BenchmarkState_Merge(b *testing.B) {
	cfg := loom.DefaultMergeConfig()
	base := loom.State{"a": "1", "b": "2", "messages": []any{"m1", "m2"}}
	update := loom.State{"c": "3", "messages": []any{"m3"}}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		base.Merge(update, cfg)
	}
}

func BenchmarkGraph_Concurrent_50(b *testing.B) {
	store := loom.NewMemStore()
	g := loom.NewGraph("bench", "s")
	g.AddStep("s", echoStep("x", "1"), loom.End())

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		for j := 0; j < 50; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				g.Run(context.Background(), loom.State{}, store)
			}()
		}
		wg.Wait()
	}
}
