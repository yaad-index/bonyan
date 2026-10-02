package eval_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/eval"
	"github.com/yaad-index/bonyan/eval/score"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/secret"
	"github.com/yaad-index/bonyan/telemetry"
)

// live is an agent recording to a queue, with telemetry going to recorders.
type live struct {
	agent   agent.Agent
	queue   *record.MemQueue
	tel     *telemetry.Telemetry
	spans   *tracetest.SpanRecorder
	metrics *sdkmetric.ManualReader
}

func newLive(t *testing.T, f chat) live {
	t.Helper()
	q, err := record.NewMemQueue(time.Hour)
	require.NoError(t, err)
	rec, err := record.NewRecorder(&discard{}, secret.NewScrubber(), record.WithQueue(q, record.QueueOptions{Rate: 1}))
	require.NoError(t, err)
	spans := tracetest.NewSpanRecorder()
	reader := sdkmetric.NewManualReader()
	tel, err := telemetry.New(telemetry.Options{
		TracerProvider: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)),
		MeterProvider:  sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)),
	})
	require.NoError(t, err)
	a := newAgent(f)
	a.Name, a.Subject, a.Recorder, a.Telemetry = "helper", "ana", rec, tel
	return live{agent: a, queue: q, tel: tel, spans: spans, metrics: reader}
}

func (l live) span(t *testing.T, name string) []sdktrace.ReadOnlySpan {
	t.Helper()
	var out []sdktrace.ReadOnlySpan
	for _, s := range l.spans.Ended() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

// Finished runs are queued, and the worker scores each after it ended and
// emits the scores attached to the run's trace.
func TestTheWorkerScoresQueuedRunsAndEmitsTheScores(t *testing.T) {
	l := newLive(t, func(_ model.ChatRequest, n int) (model.ChatResponse, error) {
		if n%2 == 0 {
			return call("search", `{"q":"x"}`, n), nil
		}
		return answer("done"), nil
	})
	for range 2 {
		_, _, err := agent.Run(context.Background(), l.agent, input("find x"))
		require.NoError(t, err)
	}
	require.Equal(t, 2, l.queue.Len())

	w := eval.Worker{Queue: l.queue, Evaluators: []score.Evaluator{eval.Outcome{}, failingEvaluator{}}, Telemetry: l.tel}
	got, err := w.Drain(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Zero(t, l.queue.Len(), "drained")
	for _, rs := range got {
		assert.Equal(t, "helper", rs.Agent)
		assert.Equal(t, []score.Score{{Evaluator: "outcome", Metric: "cleared", Value: 1}}, rs.Scores)
		assert.Equal(t, []string{"failing"}, rs.Failed)
	}

	runs := l.span(t, "invoke_agent helper")
	evals := l.span(t, "bonyan.eval")
	require.Len(t, runs, 2)
	require.Len(t, evals, 2)
	for i, ev := range evals {
		require.Len(t, ev.Links(), 1)
		assert.Equal(t, runs[i].SpanContext().TraceID(), ev.Links()[0].SpanContext.TraceID(), "linked to the run's trace")
		assert.Equal(t, runs[i].SpanContext().SpanID(), ev.Links()[0].SpanContext.SpanID(), "and to the run's span")
		keys := map[string]string{}
		for _, kv := range ev.Attributes() {
			keys[string(kv.Key)] = kv.Value.String()
		}
		assert.Equal(t, got[i].Run, keys["bonyan.run.id"])
		assert.Equal(t, "helper", keys["gen_ai.agent.name"])
		assert.Equal(t, `["failing"]`, keys["bonyan.eval.failed"])
		assert.ElementsMatch(t, []string{"bonyan.run.id", "gen_ai.agent.name", "bonyan.eval.failed"}, slices.Collect(mapKeys(keys)))
	}

	var rm metricdata.ResourceMetrics
	require.NoError(t, l.metrics.Collect(context.Background(), &rm))
	var points []metricdata.HistogramDataPoint[float64]
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "bonyan.eval.score" {
				points = append(points, m.Data.(metricdata.Histogram[float64]).DataPoints...)
			}
		}
	}
	require.Len(t, points, 1, "one series: evaluator, metric and agent, never the run")
	assert.Equal(t, uint64(2), points[0].Count)
	assert.Equal(t, 2.0, points[0].Sum)
	var keys []string
	for _, kv := range points[0].Attributes.ToSlice() {
		keys = append(keys, string(kv.Key)+"="+kv.Value.String())
	}
	assert.ElementsMatch(t, []string{"bonyan.eval.evaluator=outcome", "bonyan.eval.metric=cleared", "gen_ai.agent.name=helper"}, keys)
}

func mapKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// judging runs an agent of its own while it evaluates, recording to the same
// queue, as a model-based evaluator's judge would.
type judging struct {
	t     *testing.T
	judge agent.Agent
}

func (judging) Name() string { return "judging" }

func (j judging) Evaluate(ctx context.Context, _ score.Subject) ([]score.Score, error) {
	_, _, err := agent.Run(ctx, j.judge, input("is it grounded?"))
	return []score.Score{{Metric: "judged", Value: 1}}, err
}

// A judge's own runs are never handed to the queue, so evaluating cannot feed
// itself.
func TestEvaluatingCannotFeedTheQueue(t *testing.T) {
	l := newLive(t, func(model.ChatRequest, int) (model.ChatResponse, error) { return answer("done"), nil })
	judge := l.agent
	judge.Name = "judge"

	_, _, err := agent.Run(context.Background(), l.agent, input("find x"))
	require.NoError(t, err)
	require.Equal(t, 1, l.queue.Len())

	w := eval.Worker{Queue: l.queue, Evaluators: []score.Evaluator{judging{t: t, judge: judge}}}
	got, err := w.Drain(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].Failed, "the judge ran")
	assert.Equal(t, 1.0, mustScore(t, got[0].Scores, "judging", "judged"))
	assert.Zero(t, l.queue.Len(), "the judge's run was not queued")

	// The judge's agent is not special: run outside evaluation, it is queued.
	_, _, err = agent.Run(context.Background(), judge, input("is it grounded?"))
	require.NoError(t, err)
	assert.Equal(t, 1, l.queue.Len())

	// A Runner's own runs are evaluation too.
	_, err = eval.Runner{Agent: withoutRecorder(judge), Evaluators: []score.Evaluator{judging{t: t, judge: judge}}}.Run(context.Background(), []score.Case{{Name: "c", Input: input("x")}}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, l.queue.Len(), "the runner's judge run was not queued")
}

func withoutRecorder(a agent.Agent) agent.Agent {
	a.Recorder = nil
	return a
}

// An item that does not read as a recording is reported; the others are
// scored.
func TestTheWorkerSkipsAnItemItCannotRead(t *testing.T) {
	l := newLive(t, func(model.ChatRequest, int) (model.ChatResponse, error) { return answer("done"), nil })
	require.NoError(t, l.queue.Put(context.Background(), record.Item{Run: "broken", Subject: "ana", At: time.Now(), Recording: []byte("not a recording")}))
	_, _, err := agent.Run(context.Background(), l.agent, input("x"))
	require.NoError(t, err)

	got, err := eval.Worker{Queue: l.queue, Evaluators: []score.Evaluator{eval.Outcome{}}}.Drain(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"broken"`)
	require.Len(t, got, 1)
	assert.Zero(t, l.queue.Len())

	_, err = eval.Worker{Evaluators: []score.Evaluator{eval.Outcome{}}}.Drain(context.Background())
	require.Error(t, err, "no queue")
}

// A run with no subject is not queued, and that is recorded.
func TestARunWithNoSubjectIsNotQueued(t *testing.T) {
	l := newLive(t, func(model.ChatRequest, int) (model.ChatResponse, error) { return answer("done"), nil })
	l.agent.Subject = ""
	_, _, err := agent.Run(context.Background(), l.agent, input("x"))
	require.NoError(t, err)
	assert.Zero(t, l.queue.Len())
}
