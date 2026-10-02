package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/eval/score"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/secret"
)

// Runner runs an agent on a set of cases and scores each run with its
// evaluators (ADR 0001 §8). A prompt, model or threshold change is meant to
// be judged by its report before it ships.
type Runner struct {
	// Agent is run once per case. It must have no Recorder: the runner
	// records each run itself, to memory, under the same rules as a file
	// recording, and the evaluators read that recording.
	Agent agent.Agent
	// Evaluators are the agent's evaluators, all on unless a run switches one
	// off.
	Evaluators []score.Evaluator
}

// Switch turns evaluators on or off for one Run, by name. An evaluator it
// does not name stays on. Inline loop detection is the agent's, and is
// switched with agent.Agent.LoopThreshold.
type Switch map[string]bool

// Report is what a Run found.
type Report struct {
	// Cases are the results in the order the cases were given.
	Cases []Result
	// Aggregates hold each metric over every case that has it, in the order
	// the metrics first appear.
	Aggregates []Aggregate
}

// Result is one case's run and its scores.
type Result struct {
	Case    string
	Outcome agent.Outcome
	Report  agent.Report
	Scores  []score.Score
	// Failed names the evaluators that could not score the run. A failing
	// evaluator does not fail the other evaluators or the run.
	Failed []string
}

// Aggregate is one metric over the cases that have it.
type Aggregate struct {
	Evaluator string
	Metric    string
	// N is how many cases have the metric.
	N                   int
	Sum, Mean, Min, Max float64
}

// Run runs every case in order and scores each run with the evaluators sw
// leaves on. An error means the agent or the cases could not be run at all; a
// run that did not answer is a result, not an error.
func (r Runner) Run(ctx context.Context, cases []score.Case, sw Switch) (Report, error) {
	if r.Agent.Recorder != nil {
		return Report{}, errors.New("eval: the runner records each run itself; leave Agent.Recorder nil")
	}
	if err := checkNames(r.Evaluators); err != nil {
		return Report{}, err
	}
	if err := checkCases(cases); err != nil {
		return Report{}, err
	}
	on := make([]score.Evaluator, 0, len(r.Evaluators))
	known := map[string]bool{}
	for _, e := range r.Evaluators {
		known[e.Name()] = true
		if enabled, set := sw[e.Name()]; !set || enabled {
			on = append(on, e)
		}
	}
	for name := range sw {
		if !known[name] {
			return Report{}, fmt.Errorf("eval: no evaluator is named %q", name)
		}
	}
	scrub := r.Agent.Scrubber
	if scrub == nil {
		scrub = secret.NewScrubber()
	}

	var rep Report
	for i := range cases {
		c := cases[i]
		res, err := r.runCase(ctx, &c, on, scrub)
		if err != nil {
			return Report{}, fmt.Errorf("eval: case %q: %w", c.Name, err)
		}
		rep.Cases = append(rep.Cases, res)
	}
	rep.Aggregates = aggregate(rep.Cases)
	return rep, nil
}

// runCase runs the agent on c with a recording of its own and scores the run
// the recording holds.
func (r Runner) runCase(ctx context.Context, c *score.Case, evaluators []score.Evaluator, scrub *secret.Scrubber) (Result, error) {
	sink := newMemSink()
	rec, err := record.NewRecorder(sink, scrub)
	if err != nil {
		return Result{}, err
	}
	a := r.Agent
	a.Recorder = rec
	out, report, err := agent.Run(ctx, a, c.Input)
	if err != nil {
		return Result{}, err
	}
	if rec.WriteFailures() > 0 {
		return Result{}, errors.New("the run's recording is incomplete")
	}
	_, runs, err := record.ReadRuns(sink.reader())
	if err != nil {
		return Result{}, err
	}
	if len(runs) != 1 {
		return Result{}, fmt.Errorf("the run's recording holds %d runs", len(runs))
	}
	answer, answered := out.Answer()
	s := score.Subject{Run: runs[0], Case: c, Answer: answer, Answered: answered}
	scores, failed := score.Evaluate(ctx, s, evaluators...)
	return Result{Case: c.Name, Outcome: out, Report: report, Scores: scores, Failed: failed}, nil
}

// aggregate summarises each metric over the results that have it.
func aggregate(results []Result) []Aggregate {
	type key struct{ evaluator, metric string }
	var order []key
	byKey := map[key]*Aggregate{}
	for _, res := range results {
		for _, s := range res.Scores {
			k := key{s.Evaluator, s.Metric}
			a, ok := byKey[k]
			if !ok {
				a = &Aggregate{Evaluator: s.Evaluator, Metric: s.Metric, Min: s.Value, Max: s.Value}
				byKey[k] = a
				order = append(order, k)
			}
			a.N++
			a.Sum += s.Value
			a.Min = min(a.Min, s.Value)
			a.Max = max(a.Max, s.Value)
		}
	}
	out := make([]Aggregate, len(order))
	for i, k := range order {
		a := byKey[k]
		a.Mean = a.Sum / float64(a.N)
		out[i] = *a
	}
	return out
}

// PerCleared is a metric's sum over every case divided by the number of cases
// that answered: for waste's tokens or cost, what a completed task cost,
// failed runs included. It is false when the metric is absent or no case
// answered.
func (r Report) PerCleared(evaluator, metric string) (float64, bool) {
	cleared := 0
	for _, res := range r.Cases {
		if res.Outcome.Cleared() {
			cleared++
		}
	}
	if cleared == 0 {
		return 0, false
	}
	for _, a := range r.Aggregates {
		if a.Evaluator == evaluator && a.Metric == metric {
			return a.Sum / float64(cleared), true
		}
	}
	return 0, false
}

// memSink keeps a recording in memory, in the form a file holds.
type memSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
	enc *json.Encoder
}

func newMemSink() *memSink {
	s := &memSink{}
	s.enc = json.NewEncoder(&s.buf)
	// The header is what record.OpenFile writes, so the recording is read
	// back exactly as a file would be.
	_ = s.enc.Encode(record.Header{Format: record.Format, Version: record.Version})
	return s
}

func (s *memSink) Write(e record.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enc.Encode(e)
}

func (*memSink) Full() bool      { return false }
func (*memSink) Subject() string { return "" }
func (*memSink) Close() error    { return nil }

func (s *memSink) reader() *bytes.Reader {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.NewReader(bytes.Clone(s.buf.Bytes()))
}

// checkCases refuses a case set the report could not tell apart.
func checkCases(cases []score.Case) error {
	names := map[string]bool{}
	for _, c := range cases {
		if c.Name == "" {
			return errors.New("eval: a case has no name")
		}
		if names[c.Name] {
			return fmt.Errorf("eval: two cases are named %q", c.Name)
		}
		names[c.Name] = true
		props := map[string]bool{}
		for _, p := range c.Expect {
			if p.Name == "" || p.Check == nil {
				return fmt.Errorf("eval: case %q has a property with no name or no check", c.Name)
			}
			if props[p.Name] {
				return fmt.Errorf("eval: case %q has two properties named %q", c.Name, p.Name)
			}
			props[p.Name] = true
		}
	}
	return nil
}
