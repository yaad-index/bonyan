package model_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/model"
)

type failN struct {
	calls int
	fail  int
	err   error
}

func (f *failN) Chat(context.Context, model.ChatRequest) (model.ChatResponse, error) {
	f.calls++
	if f.calls <= f.fail {
		return model.ChatResponse{}, f.err
	}
	return model.ChatResponse{Content: "ok"}, nil
}

var retryable = &model.CallError{Kind: model.ErrTransport, Retryable: true}

func TestRetryRetriesOnlyRetryableErrors(t *testing.T) {
	f := &failN{fail: 2, err: retryable}
	resp, err := model.Retry(f, model.RetryPolicy{Attempts: 3}).Chat(context.Background(), model.ChatRequest{})
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Content)
	assert.Equal(t, 3, f.calls)

	f = &failN{fail: 5, err: retryable}
	_, err = model.Retry(f, model.RetryPolicy{Attempts: 3}).Chat(context.Background(), model.ChatRequest{})
	require.ErrorIs(t, err, retryable)
	assert.Equal(t, 3, f.calls, "attempts are bounded")

	for _, e := range []error{&model.CallError{Kind: model.ErrRejected}, errors.New("plain"), context.Canceled} {
		f = &failN{fail: 5, err: e}
		_, err = model.Retry(f, model.RetryPolicy{Attempts: 3}).Chat(context.Background(), model.ChatRequest{})
		require.ErrorIs(t, err, e)
		assert.Equal(t, 1, f.calls, "%v is not retried", e)
	}

	f = &failN{fail: 5, err: retryable}
	_, err = model.Retry(f, model.RetryPolicy{}).Chat(context.Background(), model.ChatRequest{})
	require.Error(t, err)
	assert.Equal(t, 1, f.calls, "no attempts configured means one call")
}

func TestRetryBackoffStopsAtTheDeadline(t *testing.T) {
	f := &failN{fail: 5, err: retryable}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := model.Retry(f, model.RetryPolicy{Attempts: 5, Backoff: time.Hour}).Chat(ctx, model.ChatRequest{})
	require.ErrorIs(t, err, retryable)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Equal(t, 1, f.calls)
}
