package cf_resend

import (
	"net/http"
	"testing"
	"time"
)

func TestShouldRetryStatus(t *testing.T) {
	if !shouldRetryStatus(429) || !shouldRetryStatus(500) || !shouldRetryStatus(503) {
		t.Fatal("429 and 5xx should retry")
	}
	if shouldRetryStatus(422) || shouldRetryStatus(400) || shouldRetryStatus(0) {
		t.Fatal("4xx other than 429 and network must not retry")
	}
}

func TestRetryWaitDuration(t *testing.T) {
	if d := retryWaitDuration(""); d != retryWaitDefault {
		t.Fatalf("empty = %v", d)
	}
	if d := retryWaitDuration("0"); d != 0 {
		t.Fatalf("0s = %v", d)
	}
	if d := retryWaitDuration("30"); d != retryWaitMax {
		t.Fatalf("30s should cap at %v, got %v", retryWaitMax, d)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if d := retryWaitDuration(past); d != 0 {
		t.Fatalf("past date = %v", d)
	}
}
