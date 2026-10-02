package eval

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/yaad-index/bonyan/budget"
	"github.com/yaad-index/bonyan/eval/score"
	"github.com/yaad-index/bonyan/registry"
)

// Register registers the evaluators this package ships in r, under their
// names: "loops" with the options {"threshold": n}, "waste" with
// {"prices": {"<model>": {"input": n, "output": n}}}, and "outcome", which
// takes none.
func Register(r *registry.Registry) error {
	return errors.Join(
		r.RegisterEvaluator(Loops{}.Name(), func(options json.RawMessage) (score.Evaluator, error) {
			var o struct {
				Threshold int `json:"threshold"`
			}
			if err := decode(options, &o); err != nil {
				return nil, err
			}
			return Loops{Threshold: o.Threshold}, nil
		}),
		r.RegisterEvaluator(Waste{}.Name(), func(options json.RawMessage) (score.Evaluator, error) {
			var o struct {
				Prices budget.PriceTable `json:"prices"`
			}
			if err := decode(options, &o); err != nil {
				return nil, err
			}
			return Waste{Prices: o.Prices}, nil
		}),
		r.RegisterEvaluator(Outcome{}.Name(), func(options json.RawMessage) (score.Evaluator, error) {
			return Outcome{}, decode(options, &struct{}{})
		}),
	)
}

// decode reads options into v, refusing a field v does not have, so a
// misspelt option is an error rather than ignored.
func decode(options json.RawMessage, v any) error {
	if len(options) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(options))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
