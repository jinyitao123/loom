package stdlib_test

import (
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

func TestSession_SaveAndLoad(t *testing.T) {
	store := loom.NewMemStore()
	msgs := []contract.Message{
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi there!"},
	}

	err := stdlib.SaveSession(store, "sess-123", msgs)
	assertNoError(t, err)

	loaded, err := stdlib.LoadSession(store, "sess-123")
	assertNoError(t, err)
	if len(loaded) != 2 {
		t.Fatalf("loaded %d messages, want 2", len(loaded))
	}
	if loaded[0].Content != "Hello" {
		t.Errorf("msg[0] = %q", loaded[0].Content)
	}
}

func TestSession_LoadNonExistent(t *testing.T) {
	store := loom.NewMemStore()
	msgs, err := stdlib.LoadSession(store, "nonexistent")
	assertNoError(t, err)
	if msgs != nil {
		t.Errorf("expected nil messages, got %v", msgs)
	}
}
