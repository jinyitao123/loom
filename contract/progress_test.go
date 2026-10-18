package contract_test

import (
	"context"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

func TestToolProgressTravelsWithTheContext(t *testing.T) {
	contract.ReportToolProgress(context.Background(), "c0", contract.ToolResult{Content: "ignored"})

	var got []string
	ctx := contract.WithToolProgress(context.Background(), func(callID string, partial contract.ToolResult) {
		got = append(got, callID+":"+partial.Content)
	})
	contract.ReportToolProgress(ctx, "c1", contract.ToolResult{Content: "line 1"})
	contract.ReportToolProgress(ctx, "c1", contract.ToolResult{Content: "line 1\nline 2"})
	if len(got) != 2 || got[1] != "c1:line 1\nline 2" {
		t.Fatalf("progress = %v", got)
	}
}
