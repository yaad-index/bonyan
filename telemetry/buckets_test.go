package telemetry_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/yaad-index/bonyan/budget"
	"github.com/yaad-index/bonyan/eval/score"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/telemetry"
)

type usageReply struct{}

func (usageReply) Chat(context.Context, model.ChatRequest) (model.ChatResponse, error) {
	return model.ChatResponse{Content: "ok", StopReason: model.StopEnd, Usage: &model.Usage{InputTokens: 3, OutputTokens: 2}}, nil
}

// Each histogram carries its bucket boundaries from its creation, on a meter
// provider with no views: model-call durations and token counts on the GenAI
// conventions' advised boundaries, and eval scores on 0.5 then doubling, so a
// 0/1 flag and a count of millions both land in a bucket of their own size.
func TestHistogramsHaveTheirBuckets(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	tel, err := telemetry.New(telemetry.Options{MeterProvider: sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))})
	require.NoError(t, err)
	_, err = tel.Chat(usageReply{}, "m", budget.Price{}, nil).Chat(context.Background(), model.ChatRequest{MaxOutputTokens: 10})
	require.NoError(t, err)
	tel.Scores(context.Background(), telemetry.EvaluatedRun{ID: "r"}, []score.Score{{Evaluator: "e", Metric: "loop", Value: 1}}, nil)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	got := map[string][]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Histogram[float64]:
				got[m.Name] = d.DataPoints[0].Bounds
			case metricdata.Histogram[int64]:
				got[m.Name] = d.DataPoints[0].Bounds
			}
		}
	}
	assert.Equal(t, []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92},
		got["gen_ai.client.operation.duration"])
	assert.Equal(t, []float64{1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864},
		got["gen_ai.client.token.usage"])
	scores := []float64{0.5}
	for v := 1.0; v <= 1<<26; v *= 2 {
		scores = append(scores, v)
	}
	require.Len(t, scores, 28)
	assert.Equal(t, scores, got["bonyan.eval.score"])
}
