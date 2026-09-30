package model

import (
	"context"
	"errors"
	"time"
)

// RetryPolicy bounds retries of a failed call.
type RetryPolicy struct {
	// Attempts is the most calls made, the first included. Values below 1
	// mean 1: no retry.
	Attempts int
	// Backoff is the wait before the first retry, doubled before each later
	// one. Zero means no wait.
	Backoff time.Duration
}

// Retry wraps a chat model so that a call failing with a retryable CallError
// is made again, up to the policy's attempts, waiting between attempts. Any
// other error, and the context ending, stop at once.
//
// Retry sits outside the wrappers that must see every attempt: stacked as
// Retry(budget(record(adapter))), each attempt is admitted and charged against
// the budget and recorded (ADR 0001 §8, §11).
func Retry(inner Chat, p RetryPolicy) Chat {
	if p.Attempts < 1 {
		p.Attempts = 1
	}
	return retrying{inner: inner, p: p}
}

type retrying struct {
	inner Chat
	p     RetryPolicy
}

func (r retrying) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	wait := r.p.Backoff
	for attempt := 1; ; attempt++ {
		resp, err := r.inner.Chat(ctx, req)
		var ce *CallError
		if err == nil || attempt >= r.p.Attempts || !errors.As(err, &ce) || !ce.Retryable {
			return resp, err
		}
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return resp, err
			case <-t.C:
			}
			wait *= 2
		}
	}
}
