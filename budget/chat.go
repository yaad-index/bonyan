package budget

import (
	"context"
	"errors"

	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/tokenize"
)

// Chat wraps a chat model so that every call is admitted before it is sent and
// charged after it returns. A refused call never reaches the model. A call
// that fails is charged its bound, unless the error is a model.CallError with
// NotSent set, meaning the request never reached the endpoint (ADR 0001 §11).
//
// TODO(phase 8, the agent loop): the loop applies this per run.
func Chat(inner model.Chat, modelName string, meter *Meter, counter tokenize.Counter) model.Chat {
	return budgetedChat{inner: inner, name: modelName, meter: meter, counter: counter}
}

type budgetedChat struct {
	inner   model.Chat
	name    string
	meter   *Meter
	counter tokenize.Counter
}

func (b budgetedChat) Chat(ctx context.Context, req model.ChatRequest) (model.ChatResponse, error) {
	if req.MaxOutputTokens <= 0 {
		return model.ChatResponse{}, ErrNoOutputCap
	}
	input, err := b.counter.Count(req)
	if err != nil {
		return model.ChatResponse{}, err
	}
	if err := b.meter.Admit(b.name, input, req.MaxOutputTokens); err != nil {
		return model.ChatResponse{}, err
	}
	resp, err := b.inner.Chat(ctx, req)
	if err != nil {
		var ce *model.CallError
		if errors.As(err, &ce) && ce.NotSent {
			return resp, err
		}
		if cerr := b.meter.ChargeBound(b.name, input, req.MaxOutputTokens); cerr != nil {
			return resp, errors.Join(err, cerr)
		}
		return resp, err
	}
	if err := b.meter.Charge(b.name, resp.Usage); err != nil {
		return resp, err
	}
	return resp, nil
}
