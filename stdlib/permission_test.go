package stdlib_test

import (
	"context"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

func TestPermission_DenyBlocks(t *testing.T) {
	inner := &mockTools{
		tools: []contract.ToolDef{{Name: "Bash"}, {Name: "Read"}, {Name: "Edit"}},
	}
	perm := stdlib.NewPermissionDispatcher(inner, []string{"Bash"}, nil)

	_, err := perm.Dispatch(context.Background(), contract.ToolCall{Name: "Bash"})
	assertError(t, err, "denied")

	_, err = perm.Dispatch(context.Background(), contract.ToolCall{Name: "Read"})
	assertNoError(t, err)
}

func TestPermission_AllowWhitelist(t *testing.T) {
	inner := &mockTools{
		tools: []contract.ToolDef{{Name: "search"}, {Name: "deploy"}, {Name: "read"}},
	}
	perm := stdlib.NewPermissionDispatcher(inner, nil, []string{"search", "read"})

	_, err := perm.Dispatch(context.Background(), contract.ToolCall{Name: "search"})
	assertNoError(t, err)

	_, err = perm.Dispatch(context.Background(), contract.ToolCall{Name: "deploy"})
	assertError(t, err, "not in allow list")
}

func TestPermission_ListToolsFiltered(t *testing.T) {
	inner := &mockTools{
		tools: []contract.ToolDef{{Name: "Bash"}, {Name: "Read"}, {Name: "Edit"}},
	}
	perm := stdlib.NewPermissionDispatcher(inner, []string{"Bash"}, nil)

	visible, _ := perm.ListTools(context.Background())
	for _, td := range visible {
		if td.Name == "Bash" {
			t.Error("Bash should be filtered from ListTools")
		}
	}
	if len(visible) != 2 {
		t.Errorf("visible tools = %d, want 2", len(visible))
	}
}

func TestPermission_DenyOverridesAllow(t *testing.T) {
	inner := &mockTools{
		tools: []contract.ToolDef{{Name: "Bash"}},
	}
	perm := stdlib.NewPermissionDispatcher(inner, []string{"Bash"}, []string{"Bash"})

	_, err := perm.Dispatch(context.Background(), contract.ToolCall{Name: "Bash"})
	assertError(t, err, "denied")
}
