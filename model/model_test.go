package model_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
)

func TestToolResultIsUntrustedWithItsCall(t *testing.T) {
	m := model.ToolResult("call-3", "42 rows")
	require.Len(t, m.Parts, 1)
	assert.Equal(t, model.RoleTool, m.Role)
	assert.Equal(t, "call-3", m.ToolCallID)
	u, ok := m.Parts[0].(content.Untrusted)
	require.True(t, ok, "a tool result must be untrusted")
	assert.Equal(t, content.Provenance{Kind: content.KindTool, ID: "call-3"}, u.Provenance())
}

func TestMissingUsageIsNotZeroUsage(t *testing.T) {
	missing := model.ChatResponse{}
	zero := model.ChatResponse{Usage: &model.Usage{}}
	assert.Nil(t, missing.Usage)
	require.NotNil(t, zero.Usage)
	assert.Equal(t, int64(0), zero.Usage.InputTokens)
}

func TestCallErrorUnwraps(t *testing.T) {
	base := context.DeadlineExceeded
	err := error(&model.CallError{Kind: model.ErrTimeout, Retryable: true, Err: base})
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	var ce *model.CallError
	require.True(t, errors.As(err, &ce))
	assert.True(t, ce.Retryable)
	assert.Contains(t, err.Error(), "timeout")
}
