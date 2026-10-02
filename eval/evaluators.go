package eval

import (
	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/budget"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
)

// Loops reports a run that repeated itself (ADR 0001 §8): the same tool call
// requested again, a request identical to one already answered, and a run
// that ended at its step limit or by inline loop detection. Its metrics:
//
//   - repeated_tool_calls: tool calls identical, by name and arguments, to one
//     requested earlier in the run;
//   - max_identical_tool_calls: how often the most repeated call was
//     requested;
//   - repeated_states: chat requests identical to one already answered;
//   - hit_step_limit: 1 when the run ended at its step limit, 0 when it ended
//     otherwise; absent when the recording holds no end;
//   - loop: 1 when any of these crossed its line, 0 otherwise.
type Loops struct {
	// Threshold is how many identical tool calls count as a loop, as
	// agent.Agent.LoopThreshold: zero means agent.DefaultLoopThreshold, and a
	// negative value leaves repeated calls out of loop.
	Threshold int
}

// Name returns "loops".
func (Loops) Name() string { return "loops" }

// Evaluate scores s.
func (l Loops) Evaluate(s Subject) []Score {
	threshold := l.Threshold
	if threshold == 0 {
		threshold = agent.DefaultLoopThreshold
	}
	repeated, most := repeatedCalls(s.Run)
	states := repeatedStates(s.Run)
	loop := states > 0 || (threshold > 0 && most >= threshold)
	out := []Score{
		{Metric: "repeated_tool_calls", Value: float64(repeated)},
		{Metric: "max_identical_tool_calls", Value: float64(most)},
		{Metric: "repeated_states", Value: float64(states)},
	}
	if end := s.Run.End; end != nil {
		hit := end.Outcome == string(agent.ReasonStepLimit)
		loop = loop || hit || end.Outcome == string(agent.ReasonLoopDetected)
		out = append(out, Score{Metric: "hit_step_limit", Value: flag(hit)})
	}
	return append(out, Score{Metric: "loop", Value: flag(loop)})
}

// Waste reports what a run spent and what it spent for nothing (ADR 0001 §8).
// Context sent but never used needs a model to judge and is not here. Its
// metrics:
//
//   - tokens and cost: what the run spent, cost in millionths of the price
//     table's unit; from the run's end, or, for a recording that holds none,
//     summed from the recorded usage, with cost only when Prices has every
//     model called;
//   - redundant_tool_calls: tool calls identical to one requested earlier;
//   - failed_tool_calls: tool calls that gave the model no result;
//   - failed_model_calls: model calls that failed;
//   - output_retries: answers sent back because they did not match the output
//     schema.
//
// Tokens and cost per completed task are an aggregate: see Report.PerCleared.
type Waste struct {
	// Prices price the recorded usage of a run whose recording holds no end.
	Prices budget.PriceTable
}

// Name returns "waste".
func (Waste) Name() string { return "waste" }

// Evaluate scores s.
func (w Waste) Evaluate(s Subject) []Score {
	var out []Score
	if end := s.Run.End; end != nil {
		out = append(out, Score{Metric: "tokens", Value: float64(end.Tokens)}, Score{Metric: "cost", Value: float64(end.Cost)})
	} else {
		tokens, cost, priced := w.spent(s.Run)
		out = append(out, Score{Metric: "tokens", Value: float64(tokens)})
		if priced {
			out = append(out, Score{Metric: "cost", Value: float64(cost)})
		}
	}
	repeated, _ := repeatedCalls(s.Run)
	var failedTools, retries, failedModels int
	for _, e := range s.Run.Events {
		switch {
		case e.Slot == record.SlotTool && e.Failure != "":
			failedTools++
		case e.Slot == record.SlotOutput && e.Decision == record.DecisionRetry:
			retries++
		}
	}
	for _, c := range s.Run.Calls {
		if c.ErrorKind != "" {
			failedModels++
		}
	}
	return append(out,
		Score{Metric: "redundant_tool_calls", Value: float64(repeated)},
		Score{Metric: "failed_tool_calls", Value: float64(failedTools)},
		Score{Metric: "failed_model_calls", Value: float64(failedModels)},
		Score{Metric: "output_retries", Value: float64(retries)},
	)
}

// spent sums a run's recorded usage, and prices it when every model called
// has a price.
func (w Waste) spent(run record.Run) (tokens, cost int64, priced bool) {
	priced = w.Prices != nil
	for _, c := range run.Calls {
		u := c.Response.Usage
		if u == nil {
			continue
		}
		tokens += u.InputTokens + u.OutputTokens
		if !priced {
			continue
		}
		p, err := w.Prices.Lookup(c.Model)
		if err != nil {
			priced = false
			continue
		}
		cost += p.Cost(model.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens})
	}
	return tokens, cost, priced
}

// Outcome reports whether a run answered, and whether the answer has the
// properties its case expects (ADR 0001 §8). Its metrics:
//
//   - cleared: 1 when the run answered, 0 when it did not; absent when the
//     recording holds no end;
//   - "property: " and a property's name: 1 when the answer has it, 0 when it
//     does not or there is no answer; only for a run a Runner made;
//   - expected: 1 when the run answered and the answer has every property its
//     case expects, 0 otherwise; only for a run a Runner made.
type Outcome struct{}

// Name returns "outcome".
func (Outcome) Name() string { return "outcome" }

// Evaluate scores s.
func (Outcome) Evaluate(s Subject) []Score {
	var out []Score
	if end := s.Run.End; end != nil {
		out = append(out, Score{Metric: "cleared", Value: flag(end.Outcome == record.OutcomeCleared)})
	}
	if s.Case == nil {
		return out
	}
	all := s.Answered
	for _, p := range s.Case.Expect {
		held := s.Answered && p.Check(s.Answer) == nil
		all = all && held
		out = append(out, Score{Metric: "property: " + p.Name, Value: flag(held)})
	}
	return append(out, Score{Metric: "expected", Value: flag(all)})
}

// repeatedCalls counts the tool calls a run's model requested that repeat an
// earlier one, and how often the most repeated call was requested.
func repeatedCalls(run record.Run) (repeated, most int) {
	seen := map[string]int{}
	for _, c := range run.Calls {
		for _, tc := range c.Response.ToolCalls {
			key := agent.CallKey(model.ToolCall{Name: tc.Name, Arguments: tc.Arguments})
			seen[key]++
			if seen[key] > 1 {
				repeated++
			}
			most = max(most, seen[key])
		}
	}
	return repeated, most
}

// repeatedStates counts the chat requests identical to one already answered.
// A request sent again after a failed call is a retry, not a repeat.
func repeatedStates(run record.Run) int {
	answered := map[string]bool{}
	n := 0
	for _, c := range run.Calls {
		if c.Kind != record.KindChat {
			continue
		}
		if answered[c.Fingerprint] {
			n++
		}
		if c.ErrorKind == "" {
			answered[c.Fingerprint] = true
		}
	}
	return n
}

func flag(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
