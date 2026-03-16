package loom_test

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinyitao123/loom"
)

const (
	enterpriseRunsTotal   = 10000
	enterpriseWorkers     = 256
	enterpriseResumeTotal = 2000
	enterpriseP95Budget   = 120 * time.Millisecond
)

func percentile95(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	cp := make([]time.Duration, len(values))
	copy(cp, values)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(float64(len(cp)-1) * 0.95)
	return cp[idx]
}

func TestEnterprise_Kernel_10KConcurrentRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping enterprise load test in short mode")
	}

	store := loom.NewMemStore()
	g := loom.NewGraph("enterprise-run", "work")
	g.AddStep("work", func(_ context.Context, state loom.State) (loom.State, error) {
		id, _ := state["id"].(int)
		return loom.State{
			"ok":     true,
			"result": fmt.Sprintf("done-%d", id),
		}, nil
	}, loom.End())

	jobs := make(chan int, enterpriseWorkers)
	latency := make([]time.Duration, enterpriseRunsTotal)
	errs := make([]error, enterpriseRunsTotal)
	results := make([]*loom.RunResult, enterpriseRunsTotal)
	var wg sync.WaitGroup

	for i := 0; i < enterpriseWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				start := time.Now()
				res, err := g.Run(context.Background(), loom.State{"id": id}, store)
				latency[id] = time.Since(start)
				results[id] = res
				errs[id] = err
			}
		}()
	}

	for i := 0; i < enterpriseRunsTotal; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	seenRunID := make(map[string]struct{}, enterpriseRunsTotal)
	for i := 0; i < enterpriseRunsTotal; i++ {
		if errs[i] != nil {
			t.Fatalf("run %d failed: %v", i, errs[i])
		}
		if results[i] == nil {
			t.Fatalf("run %d returned nil result", i)
		}
		want := fmt.Sprintf("done-%d", i)
		if got := results[i].State["result"]; got != want {
			t.Fatalf("run %d result=%v want=%s", i, got, want)
		}
		if results[i].StopReason != loom.StopCompleted {
			t.Fatalf("run %d stop reason=%q want=%q", i, results[i].StopReason, loom.StopCompleted)
		}
		if results[i].RunID == "" {
			t.Fatalf("run %d missing run id", i)
		}
		if _, exists := seenRunID[results[i].RunID]; exists {
			t.Fatalf("duplicate run id detected: %s", results[i].RunID)
		}
		seenRunID[results[i].RunID] = struct{}{}
	}

	p95 := percentile95(latency)
	if p95 > enterpriseP95Budget {
		t.Fatalf("p95 latency=%s exceeds budget=%s", p95, enterpriseP95Budget)
	}

	t.Logf("enterprise 10k run check passed: workers=%d p95=%s", enterpriseWorkers, p95)
}

func TestEnterprise_Kernel_YieldResumeDurability(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping enterprise resume durability test in short mode")
	}

	store := loom.NewMemStore()
	g := loom.NewGraph("enterprise-resume", "wait")
	g.AddStep("wait", func(_ context.Context, state loom.State) (loom.State, error) {
		if _, ok := state["approved"]; !ok {
			return loom.State{"__yield": true, "__yield_phase": "mid_step"}, nil
		}
		id := 0
		switch v := state["id"].(type) {
		case int:
			id = v
		case float64:
			id = int(v)
		}
		return loom.State{"result": fmt.Sprintf("approved-%d", id)}, nil
	}, loom.End())

	first := make([]*loom.RunResult, enterpriseResumeTotal)
	errFirst := make([]error, enterpriseResumeTotal)
	resumed := make([]*loom.RunResult, enterpriseResumeTotal)
	errResume := make([]error, enterpriseResumeTotal)

	jobs := make(chan int, enterpriseWorkers)
	var wg sync.WaitGroup
	for i := 0; i < enterpriseWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				first[id], errFirst[id] = g.Run(context.Background(), loom.State{"id": id}, store)
			}
		}()
	}
	for i := 0; i < enterpriseResumeTotal; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	for i := 0; i < enterpriseResumeTotal; i++ {
		if errFirst[i] != nil {
			t.Fatalf("initial run %d failed: %v", i, errFirst[i])
		}
		if first[i] == nil {
			t.Fatalf("initial run %d returned nil result", i)
		}
		if !first[i].Yielded {
			t.Fatalf("initial run %d expected yielded=true", i)
		}
	}

	resumeJobs := make(chan int, enterpriseWorkers)
	for i := 0; i < enterpriseWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range resumeJobs {
				runID := first[id].RunID
				resumed[id], errResume[id] = g.Resume(context.Background(), runID, loom.State{"approved": true}, store)
			}
		}()
	}
	for i := 0; i < enterpriseResumeTotal; i++ {
		resumeJobs <- i
	}
	close(resumeJobs)
	wg.Wait()

	for i := 0; i < enterpriseResumeTotal; i++ {
		if errResume[i] != nil {
			t.Fatalf("resume %d failed: %v", i, errResume[i])
		}
		if resumed[i] == nil {
			t.Fatalf("resume %d returned nil result", i)
		}
		if resumed[i].Yielded {
			t.Fatalf("resume %d expected yielded=false", i)
		}
		want := fmt.Sprintf("approved-%d", i)
		if got := resumed[i].State["result"]; got != want {
			t.Fatalf("resume %d result=%v want=%s", i, got, want)
		}
	}

	t.Logf("enterprise resume durability passed: sessions=%d", enterpriseResumeTotal)
}

func TestEnterprise_Kernel_LongSoak_NoFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping enterprise soak test in short mode")
	}

	store := loom.NewMemStore()
	g := loom.NewGraph("enterprise-soak", "work", loom.WithStepBudget(2000000))
	g.AddStep("work", func(_ context.Context, state loom.State) (loom.State, error) {
		n, _ := state["n"].(int)
		return loom.State{"n": n + 1}, nil
	}, loom.End())

	const workers = 128
	const duration = 20 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var ops atomic.Int64
	var failures atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					res, err := g.Run(context.Background(), loom.State{"n": workerID}, store)
					if err != nil || res == nil || res.StopReason != loom.StopCompleted {
						failures.Add(1)
						continue
					}
					ops.Add(1)
				}
			}
		}(i)
	}

	wg.Wait()

	totalOps := ops.Load()
	totalFailures := failures.Load()
	if totalOps == 0 {
		t.Fatal("soak produced zero successful ops")
	}
	if totalFailures > 0 {
		t.Fatalf("soak detected failures=%d over ops=%d", totalFailures, totalOps)
	}

	t.Logf("enterprise soak passed: duration=%s workers=%d ops=%d", duration, workers, totalOps)
}
