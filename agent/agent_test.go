package agent_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/agent"
)

func TestZeroOutcomeIsNotCleared(t *testing.T) {
	var o agent.Outcome
	assert.False(t, o.Cleared())
	_, ok := o.Answer()
	assert.False(t, ok)
	assert.Equal(t, agent.ReasonUnset, o.Reason())
}

func TestAnsweredIsTheOnlyClearedOutcome(t *testing.T) {
	a := agent.Answered("ok to send")
	assert.True(t, a.Cleared())
	got, ok := a.Answer()
	assert.True(t, ok)
	assert.Equal(t, "ok to send", got)

	for _, r := range []agent.Reason{
		agent.ReasonModelFailed, agent.ReasonInvalidOutput, agent.ReasonStepLimit,
		agent.ReasonBudgetLimit, agent.ReasonDeadline, agent.ReasonFallbackExhausted,
		agent.ReasonLoopDetected, agent.ReasonNotApproved,
	} {
		o := agent.NotCleared(r)
		assert.False(t, o.Cleared(), "reason %s", r)
		assert.Equal(t, r, o.Reason())
		assert.NotEqual(t, a, o)
	}
}

func TestDefaultLimitsAreValid(t *testing.T) {
	require.NoError(t, agent.DefaultLimits().Validate())
}

func TestEveryZeroOrNegativeLimitIsInvalid(t *testing.T) {
	cases := map[string]func(*agent.Limits){
		"steps zero":     func(l *agent.Limits) { l.MaxSteps = 0 },
		"steps negative": func(l *agent.Limits) { l.MaxSteps = -1 },
		"deadline zero":  func(l *agent.Limits) { l.Deadline = 0 },
		"deadline neg":   func(l *agent.Limits) { l.Deadline = -time.Second },
		"tokens zero":    func(l *agent.Limits) { l.Budget.MaxTokens = 0 },
		"tokens neg":     func(l *agent.Limits) { l.Budget.MaxTokens = -5 },
		"cost zero":      func(l *agent.Limits) { l.Budget.MaxCostMicros = 0 },
		"cost neg":       func(l *agent.Limits) { l.Budget.MaxCostMicros = -5 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			l := agent.DefaultLimits()
			mutate(&l)
			assert.ErrorIs(t, l.Validate(), agent.ErrInvalidLimits)
		})
	}
	assert.ErrorIs(t, agent.Limits{}.Validate(), agent.ErrInvalidLimits, "the zero Limits is invalid")
}
