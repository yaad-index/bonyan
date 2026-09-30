package registry

import (
	"context"
	"fmt"
)

// call runs f under ctx and reports how it failed, if it did. A panic in f is
// recovered and reported, never propagated. When ctx has already ended, f is not
// called. When ctx ends first, call returns at once; f keeps running in its goroutine and its result is discarded.
func call[T any](ctx context.Context, f func() (T, error)) (T, Failure) {
	var zero T
	if ctx.Err() != nil {
		return zero, FailDeadline
	}
	type result struct {
		v       T
		err     error
		panicky bool
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- result{panicky: true, err: fmt.Errorf("panic: %v", p)}
			}
		}()
		v, err := f()
		done <- result{v: v, err: err}
	}()

	select {
	case r := <-done:
		switch {
		case r.panicky:
			return zero, FailPanic
		case r.err != nil:
			return zero, FailError
		}
		return r.v, ""
	case <-ctx.Done():
		return zero, FailDeadline
	}
}
