// Package eval scores an agent's runs (ADR 0001 §8). An Evaluator reads one
// run's recording and returns scores; the evaluators here are the
// deterministic ones: Loops, Waste and Outcome. A Runner runs an agent on a
// set of cases and reports the scores per case and in aggregate, and
// EvaluateRecording scores the runs of a recording already written.
//
// A score never carries a run's content: it is a number, under a metric's name
// that the evaluator or the program's case chose. Switching an evaluator off
// only stops its scores; the agent's step and budget limits are not
// evaluators and stay in force.
package eval

import (
	"errors"
	"fmt"
	"io"

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

// Subject is what an evaluator scores: one run's recording and, when a Runner
// made the run, the case and the answer.
type Subject struct {
	Run record.Run
	// Case is the case the run answered; nil for a run read from a recording.
	Case *Case
	// Answer is the run's answer, as the agent returned it, and Answered
	// whether there was one. A recording alone does not hold them, so they are
	// set only when a Runner made the run.
	Answer   string
	Answered bool
}

// Evaluator scores runs. Evaluate returns the scores it can give the subject,
// and none for what the subject does not hold.
type Evaluator interface {
	// Name names the evaluator. It is how a run switches it off.
	Name() string
	Evaluate(s Subject) []Score
}

// Evaluate scores s with each evaluator, in order, and names the evaluator in
// each score.
func Evaluate(s Subject, evaluators ...Evaluator) []Score {
	var out []Score
	for _, e := range evaluators {
		for _, sc := range e.Evaluate(s) {
			sc.Evaluator = e.Name()
			out = append(out, sc)
		}
	}
	return out
}

// RunScores are the scores of one run of a recording.
type RunScores struct {
	// Run is the run's ID; empty for the entries written outside any run.
	Run string
	// Agent is the agent's name, if the run's start named one.
	Agent  string
	Scores []Score
}

// EvaluateRecording reads a recording and scores each of its runs with the
// evaluators.
func EvaluateRecording(r io.Reader, evaluators ...Evaluator) ([]RunScores, error) {
	if err := checkNames(evaluators); err != nil {
		return nil, err
	}
	_, runs, err := record.ReadRuns(r)
	if err != nil {
		return nil, err
	}
	out := make([]RunScores, len(runs))
	for i, run := range runs {
		out[i] = RunScores{Run: run.ID, Agent: run.Agent, Scores: Evaluate(Subject{Run: run}, evaluators...)}
	}
	return out, nil
}

// checkNames refuses evaluators without a name or with a name used twice.
func checkNames(evaluators []Evaluator) error {
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
