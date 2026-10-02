// Package score holds what an evaluator is and what it gives (ADR 0001 §8):
// the Evaluator interface, the Subject it scores, the Score it returns, and
// the Case and Property a run is checked against. It depends on no agent, so
// the registry can hold evaluators as a slot; the evaluators bonyan ships, and
// the runner, are in package eval.
package score

import (
	"context"
	"fmt"

	"github.com/yaad-index/bonyan/record"
)

// Score is one number an evaluator gave a run.
type Score struct {
	// Evaluator is the evaluator's name.
	Evaluator string
	// Metric names what was measured, within the evaluator.
	Metric string
	Value  float64
}

// Subject is what an evaluator scores: one run's recording and, when a runner
// made the run, the case and the answer.
type Subject struct {
	Run record.Run
	// Case is the case the run answered; nil for a run read from a recording.
	Case *Case
	// Answer is the run's answer, as the agent returned it, and Answered
	// whether there was one. A recording alone does not hold them, so they are
	// set only when a runner made the run.
	Answer   string
	Answered bool
}

// Evaluator scores runs. Evaluate returns the scores it can give the subject,
// and none for what the subject does not hold. An error means it could not
// score the subject at all, for example because a model it calls failed.
type Evaluator interface {
	// Name names the evaluator. It is how a run switches it off.
	Name() string
	Evaluate(ctx context.Context, s Subject) ([]Score, error)
}

// Evaluate scores s with each evaluator, in order, and names the evaluator in
// each score. An evaluator that returns an error or panics gives no scores and
// is named in failed; its error is not kept, since it can quote the run.
func Evaluate(ctx context.Context, s Subject, evaluators ...Evaluator) (scores []Score, failed []string) {
	for _, e := range evaluators {
		got, err := evaluate(ctx, e, s)
		if err != nil {
			failed = append(failed, e.Name())
			continue
		}
		for _, sc := range got {
			sc.Evaluator = e.Name()
			scores = append(scores, sc)
		}
	}
	return scores, failed
}

func evaluate(ctx context.Context, e Evaluator, s Subject) (scores []Score, err error) {
	defer func() {
		if p := recover(); p != nil {
			scores, err = nil, fmt.Errorf("score: evaluator %q panicked", e.Name())
		}
	}()
	return e.Evaluate(ctx, s)
}
