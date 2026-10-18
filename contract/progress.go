package contract

import "context"

// ToolProgress receives partial output of a running tool call: the whole
// output so far, not a delta, so a host can replace what it shows.
// ToolProgress 接收运行中工具调用的阶段性输出：给出截至目前的完整输出而非增量，宿主可直接替换显示。
type ToolProgress func(callID string, partial ToolResult)

type toolProgressKey struct{}

// WithToolProgress returns a context whose tool dispatches may report
// partial output to report. Dispatchers that do not report keep working.
// WithToolProgress 返回携带进度回调的 context；不上报进度的分发器不受影响。
func WithToolProgress(ctx context.Context, report ToolProgress) context.Context {
	return context.WithValue(ctx, toolProgressKey{}, report)
}

// ReportToolProgress sends partial output for callID when the context
// carries a ToolProgress, and does nothing otherwise.
// ReportToolProgress 在 context 带有进度回调时上报阶段性输出，否则什么也不做。
func ReportToolProgress(ctx context.Context, callID string, partial ToolResult) {
	if report, ok := ctx.Value(toolProgressKey{}).(ToolProgress); ok && report != nil {
		report(callID, partial)
	}
}
