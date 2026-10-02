package score_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/yaad-index/bonyan/eval/score"
)

// planted is text a failing evaluator's error carries, which must not be kept.
const planted = "PLANTED-EVAL-ERROR-91c4"

type fixed struct {
	name string
	f    func() ([]score.Score, error)
}

func (e fixed) Name() string { return e.name }
func (e fixed) Evaluate(context.Context, score.Subject) ([]score.Score, error) {
	return e.f()
}

// An evaluator that fails or panics gives no scores and is named; the others
// score as if it were not there, and every score names its evaluator.
func TestEvaluateIsolatesAFailingEvaluator(t *testing.T) {
	ok := fixed{"first", func() ([]score.Score, error) {
		return []score.Score{{Evaluator: "wrong", Metric: "m", Value: 1}}, nil
	}}
	failing := fixed{"failing", func() ([]score.Score, error) {
		return []score.Score{{Metric: "half", Value: 9}}, errors.New(planted)
	}}
	panicking := fixed{"panicking", func() ([]score.Score, error) { panic(planted) }}
	last := fixed{"last", func() ([]score.Score, error) {
		return []score.Score{{Metric: "n", Value: 2}}, nil
	}}

	scores, failed := score.Evaluate(context.Background(), score.Subject{}, ok, failing, panicking, last)
	assert.Equal(t, []score.Score{{Evaluator: "first", Metric: "m", Value: 1}, {Evaluator: "last", Metric: "n", Value: 2}}, scores)
	assert.Equal(t, []string{"failing", "panicking"}, failed)
	assert.NotContains(t, fmt.Sprint(scores, failed), planted)
}
