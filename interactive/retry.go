package interactive

import (
	"context"
	"errors"
	"regexp"
	"time"
)

// maxRetries bounds automatic retries of one model round.
const maxRetries = 3

// retryBaseDelay is the first backoff; each further retry doubles it.
var retryBaseDelay = 2 * time.Second

var transientError = regexp.MustCompile(`(?i)HTTP (408|409|425|429|5\d\d)\b|overloaded|rate.?limit|too many requests|temporarily unavailable|timeout|timed out|connection (reset|refused)|broken pipe|\bEOF\b|stream closed|stream scanner|server error`)

// IsTransient reports whether a provider error is worth retrying: rate
// limits, server errors, timeouts and dropped connections or streams. A
// request that did not fit the context window is not transient; compaction
// handles it.
// IsTransient 判断提供方错误是否值得重试：限流、服务端错误、超时、连接或流中断；
// 上下文放不下不算，由压缩处理。
func IsTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || IsContextOverflow(err) {
		return false
	}
	return transientError.MatchString(err.Error())
}

func retryDelay(retry int) time.Duration { return retryBaseDelay << retry }

func (a *Agent) autoRetryEnabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.autoRetry
}

// armRetry makes the coming retry cancellable by AbortRetry; it is armed
// before the retry is announced so an abort_retry sent in reply cannot miss it.
func (a *Agent) armRetry() chan struct{} {
	cancel := make(chan struct{})
	a.mu.Lock()
	a.retryCancel = cancel
	a.mu.Unlock()
	return cancel
}

// waitForRetry sleeps for the backoff. It returns false when the prompt was
// aborted or abort_retry cancelled the retry.
func (a *Agent) waitForRetry(ctx context.Context, cancel chan struct{}, delay time.Duration) bool {
	defer func() {
		a.mu.Lock()
		if a.retryCancel == cancel {
			a.retryCancel = nil
		}
		a.mu.Unlock()
	}()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-cancel:
		return false
	case <-ctx.Done():
		return false
	}
}

// AbortRetry cancels a retry that is waiting for its backoff; the failed
// round then ends the prompt with its error.
func (a *Agent) AbortRetry() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.retryCancel != nil {
		close(a.retryCancel)
		a.retryCancel = nil
	}
}
