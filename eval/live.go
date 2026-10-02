package eval

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/yaad-index/bonyan/eval/score"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/telemetry"
)

// Worker evaluates the runs a recorder handed to a queue (record.WithQueue),
// after they ended (ADR 0001 §8). Where and how often it runs is the
// program's choice: Drain empties the queue once.
type Worker struct {
	Queue      record.Queue
	Evaluators []score.Evaluator
	// Telemetry emits each run's scores, attached to the run's trace; nil
	// emits nothing.
	Telemetry *telemetry.Telemetry
}

// Drain takes every item from the queue, scores the run it holds, emits the
// scores and returns them. An item it cannot read is skipped and reported in
// the error; the others are still scored.
func (w Worker) Drain(ctx context.Context) ([]RunScores, error) {
	if w.Queue == nil {
		return nil, errors.New("eval: the worker has no queue")
	}
	if err := checkNames(w.Evaluators); err != nil {
		return nil, err
	}
	var out []RunScores
	var errs []error
	for {
		it, ok, err := w.Queue.Take(ctx)
		if err != nil {
			return out, errors.Join(append(errs, err)...)
		}
		if !ok {
			return out, errors.Join(errs...)
		}
		_, runs, err := record.ReadRuns(bytes.NewReader(it.Recording))
		if err != nil {
			errs = append(errs, fmt.Errorf("eval: queued run %q: %w", it.Run, err))
			continue
		}
		for _, run := range runs {
			if run.ID != it.Run {
				continue
			}
			scores, failed := score.Evaluate(ctx, score.Subject{Run: run}, w.Evaluators...)
			w.Telemetry.Scores(ctx, telemetry.EvaluatedRun{ID: run.ID, Agent: run.Agent, Trace: run.Trace, Span: run.Span}, scores, failed)
			out = append(out, RunScores{Run: run.ID, Agent: run.Agent, Scores: scores, Failed: failed})
		}
	}
}
