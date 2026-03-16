package pgstore_test

import (
	"context"
	"os"
	"testing"

	"github.com/anthropic/loom"
	"github.com/anthropic/loom/pgstore"
)

func getTestStore(t *testing.T) *pgstore.PGStore {
	t.Helper()
	connStr := os.Getenv("PG_CONN_STRING")
	if connStr == "" {
		t.Skip("PG_CONN_STRING not set, skipping PGStore integration test")
	}
	store, err := pgstore.New(connStr)
	if err != nil {
		t.Fatalf("pgstore.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Clean up test data before each test.
	ctx := context.Background()
	keys, _ := store.List(ctx, "test", "")
	for _, k := range keys {
		store.Delete(ctx, "test", k)
	}

	return store
}

func TestPGStore_PutGetDelete(t *testing.T) {
	store := getTestStore(t)
	ctx := context.Background()

	// Put
	err := store.Put(ctx, "test", "key1", []byte(`{"hello":"world"}`))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Get
	val, err := store.Get(ctx, "test", "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(val) != `{"hello":"world"}` {
		t.Errorf("Get = %s", string(val))
	}

	// Overwrite
	err = store.Put(ctx, "test", "key1", []byte(`{"updated":true}`))
	if err != nil {
		t.Fatalf("Put overwrite: %v", err)
	}
	val, err = store.Get(ctx, "test", "key1")
	if err != nil {
		t.Fatalf("Get after overwrite: %v", err)
	}
	if string(val) != `{"updated":true}` {
		t.Errorf("Get after overwrite = %s", string(val))
	}

	// Delete
	err = store.Delete(ctx, "test", "key1")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Get after delete — should error
	_, err = store.Get(ctx, "test", "key1")
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestPGStore_List(t *testing.T) {
	store := getTestStore(t)
	ctx := context.Background()

	store.Put(ctx, "test", "run:abc:1", []byte("a"))
	store.Put(ctx, "test", "run:abc:2", []byte("b"))
	store.Put(ctx, "test", "run:xyz:1", []byte("c"))

	keys, err := store.List(ctx, "test", "run:abc")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("List count = %d, want 2", len(keys))
	}

	allKeys, err := store.List(ctx, "test", "run:")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(allKeys) != 3 {
		t.Fatalf("List all count = %d, want 3", len(allKeys))
	}
}

func TestPGStore_Tx(t *testing.T) {
	store := getTestStore(t)
	ctx := context.Background()

	// Successful transaction.
	err := store.Tx(ctx, func(txStore loom.Store) error {
		if err := txStore.Put(ctx, "test", "tx-key", []byte("tx-value")); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}

	val, err := store.Get(ctx, "test", "tx-key")
	if err != nil {
		t.Fatalf("Get after tx: %v", err)
	}
	if string(val) != "tx-value" {
		t.Errorf("value = %s", string(val))
	}

	// Rolled-back transaction.
	err = store.Tx(ctx, func(txStore loom.Store) error {
		txStore.Put(ctx, "test", "tx-rollback", []byte("should-not-exist"))
		return context.Canceled // force rollback
	})
	if err == nil {
		t.Error("expected error from rolled-back tx")
	}
	_, err = store.Get(ctx, "test", "tx-rollback")
	if err == nil {
		t.Error("key should not exist after rollback")
	}
}

func TestPGStore_NestedTx(t *testing.T) {
	store := getTestStore(t)
	ctx := context.Background()

	err := store.Tx(ctx, func(txStore loom.Store) error {
		return txStore.Tx(ctx, func(_ loom.Store) error {
			return nil
		})
	})
	if err != loom.ErrNestedTx {
		t.Errorf("expected ErrNestedTx, got %v", err)
	}
}

func TestPGStore_GetNotFound(t *testing.T) {
	store := getTestStore(t)
	ctx := context.Background()

	_, err := store.Get(ctx, "test", "nonexistent-key-12345")
	if err == nil {
		t.Error("expected error for nonexistent key")
	}
}
