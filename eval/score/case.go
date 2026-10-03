package score

import (
	"errors"
	"fmt"
	"strings"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/tool"
)

// Case is one input to run the agent on, and what its answer is expected to
// be.
type Case struct {
	// Name names the case in the report. Every case in a set has its own.
	Name  string
	Input content.Untrusted
	// Expect are the properties the answer must have. Outcome scores each.
	Expect []Property
	// Labels are the values a person gave the case's run, so a model-based
	// evaluator's agreement with people is reported beside its scores
	// (ADR 0001 §8). Each names an evaluator and a metric, once per case.
	Labels []Label
}

// Label is the value a person gave one metric of an evaluator for a case's
// run.
type Label struct {
	Evaluator, Metric string
	Value             float64
}

// Property is something an answer is expected to have. Check returns nil when
// the answer has it. Its error is not reported, since it can quote the
// answer; the score says only whether the property held.
type Property struct {
	// Name names the property in its score. Every property of a case has its
	// own.
	Name  string
	Check func(answer string) error
}

// Equals is the property of being exactly want.
func Equals(want string) Property {
	return Property{Name: fmt.Sprintf("equals %q", want), Check: func(answer string) error {
		if answer != want {
			return errors.New("the answer differs")
		}
		return nil
	}}
}

// Contains is the property of containing part.
func Contains(part string) Property {
	return Property{Name: fmt.Sprintf("contains %q", part), Check: func(answer string) error {
		if !strings.Contains(answer, part) {
			return errors.New("the answer does not contain it")
		}
		return nil
	}}
}

// ValidAgainst is the property of being JSON that matches schema, named name.
func ValidAgainst(name string, schema tool.Schema) Property {
	return Property{Name: "valid against " + name, Check: func(answer string) error {
		return schema.Validate([]byte(answer))
	}}
}
