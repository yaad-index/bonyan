package budget_test

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/budget"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/tokenize"
)

var prices = budget.PriceTable{
	"small": {Input: 1_000_000, Output: 2_000_000}, // 1 and 2 micros per token
	"free":  {},
}

// recordingChat counts calls and answers with fixed usage.
type recordingChat struct {
	calls int
	usage *model.Usage
}

func (r *recordingChat) Chat(context.Context, model.ChatRequest) (model.ChatResponse, error) {
	r.calls++
	return model.ChatResponse{Content: "ok", StopReason: model.StopEnd, Usage: r.usage}, nil
}

func userMsg(s string) model.Message {
	return model.Message{Role: model.RoleUser, Parts: []content.Text{content.From(content.Provenance{Kind: content.KindUser}, s)}}
}

func TestCallWhoseBoundCrossesTheCeilingIsRefusedBeforeItIsSent(t *testing.T) {
	counter := tokenize.ByteBound{}
	req := model.ChatRequest{Messages: []model.Message{userMsg(strings.Repeat("a", 100))}, MaxOutputTokens: 50}
	bound, err := counter.Count(req)
	require.NoError(t, err)
	need := bound + 50

	// One token short of the bound: refused, and the model is never called.
	m, err := budget.NewMeter(need-1, 1_000_000, prices)
	require.NoError(t, err)
	inner := &recordingChat{usage: &model.Usage{InputTokens: 1, OutputTokens: 1}}
	_, err = budget.Chat(inner, "small", m, counter).Chat(context.Background(), req)
	require.ErrorIs(t, err, budget.ErrExceeded)
	assert.Zero(t, inner.calls)
	tokens, cost := m.Spent()
	assert.Zero(t, tokens, "the bound is never charged")
	assert.Zero(t, cost)

	// Exactly the bound: admitted, and charged what the model reported.
	m, err = budget.NewMeter(need, 1_000_000, prices)
	require.NoError(t, err)
	_, err = budget.Chat(inner, "small", m, counter).Chat(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, 1, inner.calls)
	tokens, cost = m.Spent()
	assert.Equal(t, int64(2), tokens)
	assert.Equal(t, int64(3), cost, "1 input token at 1 micro + 1 output token at 2")

	// The cost ceiling refuses on its own too.
	m, err = budget.NewMeter(1_000_000, need-1, budget.PriceTable{"small": {Input: 1_000_000, Output: 1_000_000}})
	require.NoError(t, err)
	err = m.Admit("small", bound, 50)
	require.ErrorIs(t, err, budget.ErrExceeded)
}

func TestMissingUsageFailsABudgetedCall(t *testing.T) {
	m, err := budget.NewMeter(1_000_000, 1_000_000, prices)
	require.NoError(t, err)
	inner := &recordingChat{usage: nil}
	req := model.ChatRequest{Messages: []model.Message{userMsg("hi")}, MaxOutputTokens: 10}

	_, err = budget.Chat(inner, "free", m, tokenize.ByteBound{}).Chat(context.Background(), req)
	require.ErrorIs(t, err, model.ErrMissingUsage, "even a free model must report usage")
	assert.Equal(t, 1, inner.calls)

	require.ErrorIs(t, m.Charge("small", nil), model.ErrMissingUsage)
	require.NoError(t, m.Charge("small", &model.Usage{}), "reported zero is not missing")
}

func TestCallWithoutOutputCapIsRefused(t *testing.T) {
	m, err := budget.NewMeter(1_000_000, 1_000_000, prices)
	require.NoError(t, err)
	inner := &recordingChat{usage: &model.Usage{}}
	_, err = budget.Chat(inner, "small", m, tokenize.ByteBound{}).Chat(context.Background(), model.ChatRequest{})
	require.ErrorIs(t, err, budget.ErrNoOutputCap)
	assert.Zero(t, inner.calls)
	require.ErrorIs(t, m.Admit("small", 1, 0), budget.ErrNoOutputCap)
	require.ErrorIs(t, m.Admit("small", 1, -1), budget.ErrNoOutputCap)
}

func TestUnpricedModelIsRefusedNotFree(t *testing.T) {
	m, err := budget.NewMeter(1_000_000, 1_000_000, prices)
	require.NoError(t, err)
	require.ErrorIs(t, m.Admit("unknown", 1, 1), budget.ErrUnpriced)
	require.ErrorIs(t, m.Charge("unknown", &model.Usage{InputTokens: 1}), budget.ErrUnpriced)

	_, err = budget.PriceTable{"neg": {Input: -1}}.Lookup("neg")
	require.Error(t, err)
}

func TestChargeCrossingTheCeilingIsReportedAndStillCharged(t *testing.T) {
	m, err := budget.NewMeter(10, 1_000_000, prices)
	require.NoError(t, err)
	require.NoError(t, m.Charge("small", &model.Usage{InputTokens: 4, OutputTokens: 4}))
	err = m.Charge("small", &model.Usage{InputTokens: 2, OutputTokens: 1})
	require.ErrorIs(t, err, budget.ErrExceeded)
	tokens, _ := m.Spent()
	assert.Equal(t, int64(11), tokens)
	require.ErrorIs(t, m.Admit("small", 0, 1), budget.ErrExceeded, "nothing is admitted after the ceiling is crossed")
}

func TestCostRoundsUpAndSaturates(t *testing.T) {
	// 1 token at 1 micro per million tokens costs a fraction of a micro, charged as 1.
	m, err := budget.NewMeter(math.MaxInt64, math.MaxInt64, budget.PriceTable{"tiny": {Input: 1, Output: 1}})
	require.NoError(t, err)
	require.NoError(t, m.Charge("tiny", &model.Usage{InputTokens: 1}))
	_, cost := m.Spent()
	assert.Equal(t, int64(1), cost)

	// Huge usage at a huge price saturates instead of wrapping to a small or
	// negative cost.
	m, err = budget.NewMeter(math.MaxInt64, math.MaxInt64-1, budget.PriceTable{"huge": {Input: math.MaxInt64, Output: math.MaxInt64}})
	require.NoError(t, err)
	err = m.Charge("huge", &model.Usage{InputTokens: math.MaxInt64, OutputTokens: math.MaxInt64})
	require.ErrorIs(t, err, budget.ErrExceeded)
	tokens, cost := m.Spent()
	assert.Equal(t, int64(math.MaxInt64), tokens)
	assert.Equal(t, int64(math.MaxInt64), cost)
	require.ErrorIs(t, m.Admit("huge", math.MaxInt64, math.MaxInt32), budget.ErrExceeded)
}

func TestInvalidInput(t *testing.T) {
	_, err := budget.NewMeter(0, 1, prices)
	require.Error(t, err)
	_, err = budget.NewMeter(1, -1, prices)
	require.Error(t, err)

	m, err := budget.NewMeter(100, 100, prices)
	require.NoError(t, err)
	require.Error(t, m.Admit("small", -5, 1))
	require.Error(t, m.Charge("small", &model.Usage{InputTokens: -1}))
	tokens, _ := m.Spent()
	assert.Zero(t, tokens, "negative usage is never charged as a refund")
}
