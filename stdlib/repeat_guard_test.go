package stdlib_test

import (
	"context"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

func TestToolRepeatGuardReturnsObservationBeforeAnotherEffect(t *testing.T) {
	hook := stdlib.NewToolRepeatGuard(2)
	call := contract.ToolCall{ID: "call", Name: "search", Args: `{"q":"same"}`}
	for index := 0; index < 2; index++ {
		if _, err := hook.Pre(context.Background(), call); err != nil {
			t.Fatalf("call %d blocked early: %v", index, err)
		}
		if err := hook.Post(context.Background(), call, &contract.ToolResult{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := hook.Pre(context.Background(), call); err == nil {
		t.Fatal("third identical call was not blocked")
	}
	changed := call
	changed.Args = `{"q":"different"}`
	if _, err := hook.Pre(context.Background(), changed); err != nil {
		t.Fatalf("changed action was blocked: %v", err)
	}
}
