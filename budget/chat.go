package budget

import (
	"context"

	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/tokenize"
)

// Chat wraps a chat model so that every call is admitted before it is sent and
// charged after it returns. A refused call never reaches the model. A call that
// fails reports no usage and is not charged; the loop's step limit, not the
// budget, bounds how many failed calls a run can make.
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
		return resp, err
	}
	if err := b.meter.Charge(b.name, resp.Usage); err != nil {
		return resp, err
	}
	return resp, nil
}
