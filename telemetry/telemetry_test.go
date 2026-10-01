package telemetry_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/yaad-index/bonyan/budget"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/telemetry"
	"github.com/yaad-index/bonyan/tool"
)

// error.type is a kind from a fixed set, found through wrapping, and never the
// error's text.
func TestErrorType(t *testing.T) {
	for err, want := range map[error]string{
		context.DeadlineExceeded: "timeout",
		context.Canceled:         "canceled",
		budget.ErrExceeded:       "budget_exceeded",
		budget.ErrUnpriced:       "unpriced",
		model.ErrMissingUsage:    "missing_usage",
		tool.ErrUnknown:          "unknown_tool",
		tool.ErrInvalidArguments: "invalid_arguments",
		errors.New("say hi"):     "_OTHER",
	} {
		assert.Equal(t, want, telemetry.ErrorType(fmt.Errorf("wrapped: %w", err)), err.Error())
	}
}

// A nil Telemetry emits nothing and passes calls through.
func TestNilTelemetry(t *testing.T) {
	var tel *telemetry.Telemetry
	ctx := context.Background()
	got, end := tel.Run(ctx, "a")
	assert.Equal(t, ctx, got)
	end("cleared", true)
	got, endStep := tel.Step(ctx, 1)
	assert.Equal(t, ctx, got)
	endStep()
	got, endTool := tel.Tool(ctx, model.ToolCall{Name: "t"}, nil)
	assert.Equal(t, ctx, got)
	endTool("", nil)
	var c model.Chat
	assert.Nil(t, tel.Chat(c, "m", budget.Price{}, nil))
}
