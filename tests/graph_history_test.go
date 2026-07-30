package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/jinyitao123/loom"
)

// storedHistoryCheckpoint mirrors the persisted fields needed by black-box tests.
// storedHistoryCheckpoint 镜像黑盒测试所需的落盘字段；生产类型保持未导出，测试只验证稳定 JSON 契约。
type storedHistoryCheckpoint struct {
	Seq       int64      `json:"seq"`        // 步序号：验证历史链跨恢复连续递增
	ParentRun string     `json:"parent_run"` // 源运行：验证 fork 溯源信息随检查点落盘
	ParentSeq int64      `json:"parent_seq"` // 源步序号：验证 fork 从指定历史节点派生
	LastStep  string     `json:"last_step"`  // 最后步骤：验证 latest 指针仍指向最新完成步骤
	State     loom.State `json:"state"`      // 状态快照：验证输入合并及 fork 协议键
}

func TestHistoryDisabledByDefault(t *testing.T) {
	ctx := context.Background()
	store := loom.NewMemStore()
	g := loom.NewGraph("history-disabled", "a")
	g.AddStep("a", echoStep("a", "true"), loom.Always("b"))
	g.AddStep("b", echoStep("b", "true"), loom.End())

	result, err := g.Run(ctx, loom.State{}, store)
	assertNoError(t, err)

	// 默认关闭历史时不得产生 runID/ 前缀键，但 latest 覆盖指针仍须存在并指向最后一步。
	keys, err := store.List(ctx, "checkpoint:"+g.Name, result.RunID+"/")
	assertNoError(t, err)
	if len(keys) != 0 {
		t.Fatalf("history keys = %v, want none", keys)
	}
	latest := decodeStoredCheckpoint(t, store, g.Name, result.RunID)
	if latest.LastStep != "b" {
		t.Fatalf("latest last_step = %q, want b", latest.LastStep)
	}
}

func TestHistoryChain(t *testing.T) {
	ctx := context.Background()
	store := loom.NewMemStore()
	g := loom.NewGraph("history-chain", "a", loom.WithCheckpointHistory(-1))
	g.AddStep("a", echoStep("a", "true"), loom.Always("b"))
	g.AddStep("b", echoStep("b", "true"), loom.Always("c"))
	g.AddStep("c", echoStep("c", "true"), loom.End())

	result, err := g.Run(ctx, loom.State{}, store)
	assertNoError(t, err)
	history, err := g.History(ctx, store, result.RunID)
	assertNoError(t, err)

	// 零填充键由 Store 按字典序列出，History 对外应呈现从 1 开始的时间升序链。
	if len(history) != 3 {
		t.Fatalf("history length = %d, want 3", len(history))
	}
	for i, wantStep := range []string{"a", "b", "c"} {
		wantSeq := int64(i + 1)
		if history[i].Seq != wantSeq || history[i].LastStep != wantStep {
			t.Errorf("history[%d] = seq %d step %q, want seq %d step %q",
				i, history[i].Seq, history[i].LastStep, wantSeq, wantStep)
		}
		if history[i].SavedAt.IsZero() {
			t.Errorf("history[%d].SavedAt is zero", i)
		}
	}
}

func TestHistoryRetention(t *testing.T) {
	ctx := context.Background()
	store := loom.NewMemStore()
	g := loom.NewGraph("history-retention", "a", loom.WithCheckpointHistory(2))
	g.AddStep("a", echoStep("a", "true"), loom.Always("b"))
	g.AddStep("b", echoStep("b", "true"), loom.Always("c"))
	g.AddStep("c", echoStep("c", "true"), loom.End())

	result, err := g.Run(ctx, loom.State{}, store)
	assertNoError(t, err)
	history, err := g.History(ctx, store, result.RunID)
	assertNoError(t, err)

	// keep=2 淘汰最老的 seq=1，只留下最近两个完成步骤。
	if len(history) != 2 {
		t.Fatalf("history length = %d, want 2", len(history))
	}
	if history[0].Seq != 2 || history[1].Seq != 3 {
		t.Fatalf("retained seqs = [%d %d], want [2 3]", history[0].Seq, history[1].Seq)
	}
}

func TestSeqContinuesAcrossResume(t *testing.T) {
	ctx := context.Background()
	store := loom.NewMemStore()
	g := loom.NewGraph("history-resume-seq", "wait", loom.WithCheckpointHistory(-1))
	g.AddStep("wait", func(_ context.Context, state loom.State) (loom.State, error) {
		if state["approved"] == true {
			return loom.State{"wait_done": true}, nil
		}
		return loom.State{"__yield": true, "__yield_phase": "mid_step"}, nil
	}, loom.Always("done"))
	g.AddStep("done", echoStep("done", "true"), loom.End())

	first, err := g.Run(ctx, loom.State{}, store)
	assertNoError(t, err)
	resumed, err := g.Resume(ctx, first.RunID, loom.State{"approved": true}, store)
	assertNoError(t, err)
	if resumed.RunID != first.RunID {
		t.Fatalf("Resume RunID = %q, want %q", resumed.RunID, first.RunID)
	}

	// 首次 yield、重跑 wait、执行 done 共三次落盘，序号必须跨 Resume 连续为 1..3。
	history, err := g.History(ctx, store, first.RunID)
	assertNoError(t, err)
	if len(history) != 3 {
		t.Fatalf("history length = %d, want 3", len(history))
	}
	for i, item := range history {
		if item.Seq != int64(i+1) {
			t.Errorf("history[%d].Seq = %d, want %d", i, item.Seq, i+1)
		}
	}
}

func TestResumeAtFork(t *testing.T) {
	ctx := context.Background()
	store := loom.NewMemStore()
	g := loom.NewGraph("history-fork", "a", loom.WithCheckpointHistory(-1))
	g.AddStep("a", echoStep("a", "true"), loom.Always("b"))
	g.AddStep("b", echoStep("b", "true"), loom.Always("c"))
	g.AddStep("c", echoStep("c", "true"), loom.End())

	source, err := g.Run(ctx, loom.State{"source": "original"}, store)
	assertNoError(t, err)
	before := snapshotRunKeys(t, ctx, store, g.Name, source.RunID)

	fork, err := g.ResumeAt(ctx, source.RunID, 1, loom.State{"fork_input": "merged"}, store)
	assertNoError(t, err)
	if fork.RunID == source.RunID || fork.RunID == "" {
		t.Fatalf("fork RunID = %q, must be new and non-empty", fork.RunID)
	}
	if fork.State["fork_input"] != "merged" {
		t.Fatalf("fork input was not merged: %#v", fork.State["fork_input"])
	}
	if fork.State["__parent_run"] != source.RunID || fork.State["__parent_seq"] != int64(1) {
		t.Fatalf("fork provenance state = (%#v, %#v), want (%q, 1)",
			fork.State["__parent_run"], fork.State["__parent_seq"], source.RunID)
	}

	// 源 seq=1 是 a 完成后的快照，fork 应从 b 开始并续编为 seq=2、3。
	history, err := g.History(ctx, store, fork.RunID)
	assertNoError(t, err)
	if len(history) != 2 || history[0].Seq != 2 || history[1].Seq != 3 {
		t.Fatalf("fork history = %#v, want seqs [2 3]", history)
	}
	forkKeys, err := store.List(ctx, "checkpoint:"+g.Name, fork.RunID+"/")
	assertNoError(t, err)
	for _, key := range forkKeys {
		cp := decodeStoredCheckpoint(t, store, g.Name, key)
		if cp.ParentRun != source.RunID || cp.ParentSeq != 1 {
			t.Errorf("fork checkpoint %q provenance = (%q, %d), want (%q, 1)",
				key, cp.ParentRun, cp.ParentSeq, source.RunID)
		}
	}

	// ResumeAt 只能向新 runID 写入；源 latest 与全部历史条目的键集合、字节内容均不可变化。
	after := snapshotRunKeys(t, ctx, store, g.Name, source.RunID)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("source run changed after fork\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func TestResumeAtAfterStepSemantics(t *testing.T) {
	ctx := context.Background()
	store := loom.NewMemStore()
	aExecutions := 0
	bExecutions := 0
	g := loom.NewGraph("history-after-step", "a", loom.WithCheckpointHistory(-1))
	g.AddStep("a", func(_ context.Context, _ loom.State) (loom.State, error) {
		aExecutions++
		return loom.State{"a_done": true}, nil
	}, loom.Always("b"))
	g.AddStep("b", func(_ context.Context, _ loom.State) (loom.State, error) {
		bExecutions++
		return loom.State{"b_done": true}, nil
	}, loom.End())

	source, err := g.Run(ctx, loom.State{}, store)
	assertNoError(t, err)
	_, err = g.ResumeAt(ctx, source.RunID, 1, loom.State{}, store)
	assertNoError(t, err)

	// 普通历史条目的 phase 为空但语义是 after_step：a 不重跑，直接由 a 的 router 进入 b。
	if aExecutions != 1 || bExecutions != 2 {
		t.Fatalf("executions = a:%d b:%d, want a:1 b:2", aExecutions, bExecutions)
	}
}

func TestResumeAtMidStepYieldEntry(t *testing.T) {
	ctx := context.Background()
	store := loom.NewMemStore()
	executions := 0
	g := loom.NewGraph("history-mid-step", "wait", loom.WithCheckpointHistory(-1))
	g.AddStep("wait", func(_ context.Context, state loom.State) (loom.State, error) {
		executions++
		if state["answer"] == "yes" {
			return loom.State{"accepted": true}, nil
		}
		return loom.State{"__yield": true, "__yield_phase": "mid_step"}, nil
	}, loom.End())

	source, err := g.Run(ctx, loom.State{}, store)
	assertNoError(t, err)
	fork, err := g.ResumeAt(ctx, source.RunID, 1, loom.State{"answer": "yes"}, store)
	assertNoError(t, err)

	// mid_step 历史节点恢复时必须以原步骤为 fork 入口，让该步骤消费新输入并再次执行。
	if executions != 2 {
		t.Fatalf("wait executions = %d, want 2", executions)
	}
	if fork.State["accepted"] != true {
		t.Fatalf("fork accepted = %#v, want true", fork.State["accepted"])
	}
	history, err := g.History(ctx, store, fork.RunID)
	assertNoError(t, err)
	if len(history) != 1 || history[0].Seq != 2 {
		t.Fatalf("fork history = %#v, want one entry at seq 2", history)
	}
}

func TestResumeAtNotFound(t *testing.T) {
	store := loom.NewMemStore()
	g := loom.NewGraph("history-not-found", "a", loom.WithCheckpointHistory(-1))

	_, err := g.ResumeAt(context.Background(), "missing-run", 7, loom.State{}, store)
	if !errors.Is(err, loom.ErrCheckpointNotFound) {
		t.Fatalf("ResumeAt error = %v, want ErrCheckpointNotFound", err)
	}
}

func TestHistorySkipsCorrupt(t *testing.T) {
	ctx := context.Background()
	store := loom.NewMemStore()
	g := loom.NewGraph("history-corrupt", "a", loom.WithCheckpointHistory(-1))
	g.AddStep("a", echoStep("a", "true"), loom.Always("b"))
	g.AddStep("b", echoStep("b", "true"), loom.End())

	result, err := g.Run(ctx, loom.State{}, store)
	assertNoError(t, err)
	err = store.Put(ctx, "checkpoint:"+g.Name, result.RunID+"/000000000099", []byte("not-json"))
	assertNoError(t, err)

	// 单条历史损坏只应被跳过，其他合法条目仍按升序返回。
	history, err := g.History(ctx, store, result.RunID)
	assertNoError(t, err)
	if len(history) != 2 || history[0].Seq != 1 || history[1].Seq != 2 {
		t.Fatalf("history = %#v, want valid seqs [1 2]", history)
	}

	// 损坏的是追加历史而非 latest 指针，既有 Resume 恢复路径不得受影响。
	resumed, err := g.Resume(ctx, result.RunID, loom.State{"resume_input": true}, store)
	assertNoError(t, err)
	if resumed.State["resume_input"] != true {
		t.Fatalf("Resume input = %#v, want true", resumed.State["resume_input"])
	}
}

func TestLegacyCheckpointSeqZero(t *testing.T) {
	ctx := context.Background()
	store := loom.NewMemStore()
	const runID = "legacy-run"
	const graphName = "history-legacy"
	legacy := []byte(`{"run_id":"legacy-run","graph":"history-legacy","last_step":"wait","state":{"__run_id":"legacy-run","ready":true},"yield_phase":""}`)
	err := store.Put(ctx, "checkpoint:"+graphName, runID, legacy)
	assertNoError(t, err)

	g := loom.NewGraph(graphName, "wait", loom.WithCheckpointHistory(-1))
	g.AddStep("wait", echoStep("resumed", "true"), loom.End())
	result, err := g.Resume(ctx, runID, loom.State{}, store)
	assertNoError(t, err)
	if result.RunID != runID {
		t.Fatalf("Resume RunID = %q, want %q", result.RunID, runID)
	}

	// 旧 JSON 无 seq 字段时反序列化为 0；恢复后的首次新检查点必须从 seq=1 起编。
	history, err := g.History(ctx, store, runID)
	assertNoError(t, err)
	if len(history) != 1 || history[0].Seq != 1 {
		t.Fatalf("legacy resumed history = %#v, want one entry at seq 1", history)
	}
}

// decodeStoredCheckpoint reads and decodes one persisted checkpoint by exact key.
// decodeStoredCheckpoint 按精确键读取并解析检查点；失败立即终止当前测试，避免级联误报。
func decodeStoredCheckpoint(t *testing.T, store loom.Store, graphName, key string) storedHistoryCheckpoint {
	t.Helper()
	data, err := store.Get(context.Background(), "checkpoint:"+graphName, key)
	if err != nil {
		t.Fatalf("Get checkpoint %q: %v", key, err)
	}
	var cp storedHistoryCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		t.Fatalf("unmarshal checkpoint %q: %v", key, err)
	}
	return cp
}

// snapshotRunKeys captures exact bytes for the latest pointer and every history entry of one run.
// snapshotRunKeys 捕获源运行 latest 与全部历史键的逐字节快照，用于证明 fork 绝不改写源存档树。
func snapshotRunKeys(t *testing.T, ctx context.Context, store loom.Store, graphName, runID string) map[string][]byte {
	t.Helper()
	ns := "checkpoint:" + graphName
	keys, err := store.List(ctx, ns, runID)
	if err != nil {
		t.Fatalf("List source run %q: %v", runID, err)
	}
	snapshot := make(map[string][]byte, len(keys))
	for _, key := range keys {
		data, err := store.Get(ctx, ns, key)
		if err != nil {
			t.Fatalf("Get source key %q: %v", key, err)
		}
		snapshot[key] = bytes.Clone(data)
	}
	return snapshot
}
