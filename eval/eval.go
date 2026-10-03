// Package eval scores an agent's runs (ADR 0001 §8). An evaluator
// (score.Evaluator) reads one run's recording and returns scores. Loops, Waste
// and Outcome are deterministic; Groundedness and UnusedContext ask a judge,
// an agent the program configures, and their scores are reported beside the
// judge's agreement with hand labels. A Runner runs an agent on a set of
// cases and reports the scores per case and in aggregate, and
// EvaluateRecording scores the runs of a recording already written. Register
// puts the deterministic evaluators in a registry, so configuration can name
// them; a judge is an agent, so the model-based ones are built in code.
//
// A score never carries a run's content: it is a number, under a metric's name
// that the evaluator or the program's case chose. Switching an evaluator off
// only stops its scores; the agent's step and budget limits are not
// evaluators and stay in force.
package eval

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/yaad-index/bonyan/eval/score"
	"github.com/yaad-index/bonyan/record"
)

// RunScores are the scores of one run of a recording.
type RunScores struct {
	// Run is the run's ID; empty for the entries written outside any run.
	Run string
	// Agent is the agent's name, if the run's start named one.
	Agent  string
	Scores []score.Score
	// Failed names the evaluators that could not score the run.
	Failed []string
}

// EvaluateRecording reads a recording and scores each of its runs with the
// evaluators.
func EvaluateRecording(ctx context.Context, r io.Reader, evaluators ...score.Evaluator) ([]RunScores, error) {
	if err := checkNames(evaluators); err != nil {
		return nil, err
	}
	_, runs, err := record.ReadRuns(r)
	if err != nil {
		return nil, err
	}
	out := make([]RunScores, len(runs))
	for i, run := range runs {
		scores, failed := score.Evaluate(ctx, score.Subject{Run: run}, evaluators...)
		out[i] = RunScores{Run: run.ID, Agent: run.Agent, Scores: scores, Failed: failed}
	}
	return out, nil
}

// checkNames refuses evaluators without a name or with a name used twice.
func checkNames(evaluators []score.Evaluator) error {
	seen := map[string]bool{}
	for _, e := range evaluators {
		if e == nil {
			return errors.New("eval: a nil evaluator")
		}
		n := e.Name()
		if n == "" {
			return errors.New("eval: an evaluator has no name")
		}
		if seen[n] {
			return fmt.Errorf("eval: two evaluators are named %q", n)
		}
		seen[n] = true
	}
	return nil
}
