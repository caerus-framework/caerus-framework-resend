package cf_resend

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	retryWaitDefault = 200 * time.Millisecond
	retryWaitMax     = time.Second
)

func shouldRetryStatus(status int) bool {
	return status == http.StatusTooManyRequests || (status >= 500 && status <= 599)
}

// retryWaitDuration maps Retry-After to a sleep. Missing/invalid header uses
// retryWaitDefault. Parsed values are clamped to retryWaitMax so auth mail
// does not block on a 30s header.
func retryWaitDuration(retryAfter string) time.Duration {
	if retryAfter == "" {
		return retryWaitDefault
	}
	raw := strings.TrimSpace(retryAfter)
	if secs, err := strconv.Atoi(raw); err == nil && secs >= 0 {
		return clampRetryWait(time.Duration(secs) * time.Second)
	}
	if t, err := http.ParseTime(raw); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		return clampRetryWait(d)
	}
	return retryWaitDefault
}

func clampRetryWait(d time.Duration) time.Duration {
	if d > retryWaitMax {
		return retryWaitMax
	}
	return d
}

func waitRetry(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
