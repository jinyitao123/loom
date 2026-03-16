package loom_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jinyitao123/loom"
)

func TestConcurrent_MultipleGraphRuns(t *testing.T) {
	store := loom.NewMemStore()
	g := loom.NewGraph("concurrent", "work")
	g.AddStep("work", func(_ context.Context, state loom.State) (loom.State, error) {
		id := state["id"]
		time.Sleep(time.Millisecond)
		return loom.State{"result": fmt.Sprintf("done-%v", id)}, nil
	}, loom.End())

	const N = 50
	var wg sync.WaitGroup
	errs := make([]error, N)
	results := make([]*loom.RunResult, N)

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = g.Run(
				context.Background(),
				loom.State{"id": idx},
				store,
			)
		}(i)
	}
	wg.Wait()

	for i := 0; i < N; i++ {
		if errs[i] != nil {
			t.Errorf("run %d failed: %v", i, errs[i])
		}
		expected := fmt.Sprintf("done-%d", i)
		if results[i].State["result"] != expected {
			t.Errorf("run %d: result = %v, want %s", i, results[i].State["result"], expected)
		}
	}
}

func TestConcurrent_MemStore_NoRace(t *testing.T) {
	store := loom.NewMemStore()
	ctx := context.Background()

	const N = 100
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(2)
		go func(idx int) {
			defer wg.Done()
			store.Put(ctx, "ns", fmt.Sprintf("key-%d", idx), []byte(fmt.Sprintf("val-%d", idx)))
		}(i)
		go func(idx int) {
			defer wg.Done()
			store.Get(ctx, "ns", fmt.Sprintf("key-%d", idx))
		}(i)
	}
	wg.Wait()
}
