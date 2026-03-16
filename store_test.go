package loom_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/anthropic/loom"
)

func StoreTestSuite(t *testing.T, store loom.Store) {
	ctx := context.Background()

	t.Run("PutAndGet", func(t *testing.T) {
		err := store.Put(ctx, "ns1", "key1", []byte(`{"v":1}`))
		assertNoError(t, err)

		val, err := store.Get(ctx, "ns1", "key1")
		assertNoError(t, err)
		if string(val) != `{"v":1}` {
			t.Errorf("got %s", val)
		}
	})

	t.Run("GetNotFound", func(t *testing.T) {
		_, err := store.Get(ctx, "ns1", "nonexistent")
		if err == nil {
			t.Error("expected error for missing key")
		}
	})

	t.Run("PutOverwrite", func(t *testing.T) {
		store.Put(ctx, "ns1", "ow", []byte("v1"))
		store.Put(ctx, "ns1", "ow", []byte("v2"))
		val, _ := store.Get(ctx, "ns1", "ow")
		if string(val) != "v2" {
			t.Errorf("got %s, want v2", val)
		}
	})

	t.Run("NamespaceIsolation", func(t *testing.T) {
		store.Put(ctx, "nsA", "key", []byte("A"))
		store.Put(ctx, "nsB", "key", []byte("B"))

		valA, _ := store.Get(ctx, "nsA", "key")
		valB, _ := store.Get(ctx, "nsB", "key")
		if string(valA) != "A" {
			t.Errorf("nsA got %s", valA)
		}
		if string(valB) != "B" {
			t.Errorf("nsB got %s", valB)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		store.Put(ctx, "ns1", "del", []byte("x"))
		store.Delete(ctx, "ns1", "del")
		_, err := store.Get(ctx, "ns1", "del")
		if err == nil {
			t.Error("expected error after delete")
		}
	})

	t.Run("List", func(t *testing.T) {
		store.Put(ctx, "list-ns", "agent:alpha", []byte("1"))
		store.Put(ctx, "list-ns", "agent:beta", []byte("2"))
		store.Put(ctx, "list-ns", "conv:123", []byte("3"))

		keys, err := store.List(ctx, "list-ns", "agent:")
		assertNoError(t, err)
		if len(keys) != 2 {
			t.Fatalf("got %d keys, want 2: %v", len(keys), keys)
		}
	})

	t.Run("ListEmpty", func(t *testing.T) {
		keys, err := store.List(ctx, "empty-ns", "")
		assertNoError(t, err)
		if len(keys) != 0 {
			t.Errorf("got %d keys, want 0", len(keys))
		}
	})

	t.Run("TxCommit", func(t *testing.T) {
		err := store.Tx(ctx, func(txs loom.Store) error {
			txs.Put(ctx, "tx-ns", "k1", []byte("v1"))
			txs.Put(ctx, "tx-ns", "k2", []byte("v2"))
			return nil
		})
		assertNoError(t, err)

		v1, _ := store.Get(ctx, "tx-ns", "k1")
		v2, _ := store.Get(ctx, "tx-ns", "k2")
		if string(v1) != "v1" || string(v2) != "v2" {
			t.Errorf("tx commit failed: k1=%s, k2=%s", v1, v2)
		}
	})

	t.Run("TxRollback", func(t *testing.T) {
		store.Tx(ctx, func(txs loom.Store) error {
			txs.Put(ctx, "tx-ns", "rollback-key", []byte("should-not-persist"))
			return fmt.Errorf("abort")
		})

		_, err := store.Get(ctx, "tx-ns", "rollback-key")
		if err == nil {
			t.Error("key should not exist after rollback")
		}
	})

	t.Run("TxNestedError", func(t *testing.T) {
		err := store.Tx(ctx, func(txs loom.Store) error {
			return txs.Tx(ctx, func(inner loom.Store) error {
				return nil
			})
		})
		if err == nil {
			t.Error("nested Tx should return error")
		}
	})
}

func TestMemStore(t *testing.T) {
	store := loom.NewMemStore()
	StoreTestSuite(t, store)
}
