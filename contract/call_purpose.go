package contract

import "context"

// CallPurpose names why a model call was made, so a host can attribute each
// call's usage. It travels in the context, not in the request, so requests
// and their identities are unchanged.
// CallPurpose 标明一次模型调用的用途，供宿主按调用归属用量；它随 context 传递而不进入请求。
type CallPurpose string

const (
	CallPurposeGeneration CallPurpose = "generation" // a normal turn of an agent loop
	CallPurposeWrapUp     CallPurpose = "wrap_up"    // the final answer requested when a loop's tool budget ran out
	CallPurposeCompaction CallPurpose = "compaction" // summarizing earlier context to fit the window
)

type callPurposeKey struct{}

// WithCallPurpose marks the model calls made with ctx; the innermost mark wins.
func WithCallPurpose(ctx context.Context, purpose CallPurpose) context.Context {
	return context.WithValue(ctx, callPurposeKey{}, purpose)
}

// CallPurposeOf reports the purpose marked on ctx, or "" when none is.
func CallPurposeOf(ctx context.Context) CallPurpose {
	purpose, _ := ctx.Value(callPurposeKey{}).(CallPurpose)
	return purpose
}
