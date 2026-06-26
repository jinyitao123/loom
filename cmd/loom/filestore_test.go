package main

import (
	"context"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

func TestFileStorePutGet(t *testing.T) {
	s := newFileStore(t.TempDir())
	if err := s.Put(context.Background(), "session", "k1", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(context.Background(), "session", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestFileStoreGetMissingIsError(t *testing.T) {
	s := newFileStore(t.TempDir())
	if _, err := s.Get(context.Background(), "session", "nope"); err == nil {
		t.Fatal("missing key should error (so stdlib.LoadSession treats it as empty)")
	}
}

func TestFileStoreDelete(t *testing.T) {
	s := newFileStore(t.TempDir())
	_ = s.Put(context.Background(), "ns", "k", []byte("v"))
	if err := s.Delete(context.Background(), "ns", "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), "ns", "k"); err == nil {
		t.Fatal("deleted key should be gone")
	}
	// deleting a missing key is not an error
	if err := s.Delete(context.Background(), "ns", "missing"); err != nil {
		t.Fatalf("delete missing should be nil, got %v", err)
	}
}

func TestFileStoreListPrefix(t *testing.T) {
	s := newFileStore(t.TempDir())
	_ = s.Put(context.Background(), "session", "ab1", []byte("1"))
	_ = s.Put(context.Background(), "session", "ac2", []byte("2"))
	_ = s.Put(context.Background(), "session", "xy3", []byte("3"))
	keys, err := s.List(context.Background(), "session", "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("want 2 keys with prefix a, got %v", keys)
	}
}

func TestFileStoreListMissingNamespace(t *testing.T) {
	s := newFileStore(t.TempDir())
	keys, err := s.List(context.Background(), "nope", "")
	if err != nil || keys != nil {
		t.Fatalf("missing ns should yield (nil, nil), got %v %v", keys, err)
	}
}

func TestFileStoreTxRunsAndRejectsNesting(t *testing.T) {
	s := newFileStore(t.TempDir())
	err := s.Tx(context.Background(), func(tx loom.Store) error {
		return tx.Put(context.Background(), "ns", "k", []byte("v"))
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(context.Background(), "ns", "k")
	if string(got) != "v" {
		t.Fatalf("Tx write not persisted: %q", got)
	}
	// nested Tx must error
	err = s.Tx(context.Background(), func(tx loom.Store) error {
		return tx.Tx(context.Background(), func(loom.Store) error { return nil })
	})
	if err == nil {
		t.Fatal("nested Tx must return an error")
	}
}

// Round-trips through the stdlib session helpers — the real usage from runAgent.
func TestFileStoreSessionRoundtrip(t *testing.T) {
	s := newFileStore(t.TempDir())
	msgs := []contract.Message{{Role: "user", Content: "first"}, {Role: "assistant", Content: "hi"}}
	if err := stdlib.SaveSession(s, "sess1", msgs); err != nil {
		t.Fatal(err)
	}
	got, err := stdlib.LoadSession(s, "sess1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Content != "first" || got[1].Content != "hi" {
		t.Fatalf("session roundtrip mismatch: %+v", got)
	}
	// a never-saved session loads as empty (not an error)
	empty, err := stdlib.LoadSession(s, "never")
	if err != nil || empty != nil {
		t.Fatalf("missing session should be (nil,nil): %v %v", empty, err)
	}
}
