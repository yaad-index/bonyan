package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/hook"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/secret"
	"github.com/yaad-index/bonyan/tool"
)

// entrySink keeps every recorded entry as the JSON a file would hold.
type entrySink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newEntrySink(t *testing.T) *entrySink {
	t.Helper()
	s := &entrySink{}
	require.NoError(t, json.NewEncoder(&s.buf).Encode(record.Header{Format: record.Format, Version: record.Version}))
	return s
}

func (s *entrySink) Write(e record.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.NewEncoder(&s.buf).Encode(e)
}
func (*entrySink) Full() bool      { return false }
func (*entrySink) Subject() string { return "" }
func (*entrySink) Close() error    { return nil }

func (s *entrySink) raw() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *entrySink) runs(t *testing.T) []record.Run {
	t.Helper()
	_, runs, err := record.ReadRuns(bytes.NewReader([]byte(s.raw())))
	require.NoError(t, err)
	return runs
}

// recorded records a's runs to a new sink.
func recorded(t *testing.T, a *agent.Agent) *entrySink {
	t.Helper()
	sink := newEntrySink(t)
	rec, err := record.NewRecorder(sink, secret.NewScrubber())
	require.NoError(t, err)
	a.Recorder = rec
	return sink
}

// toolEvents are a run's tool events, as "name:failure".
func toolEvents(run record.Run) []string {
	var out []string
	for _, e := range run.Events {
		if e.Slot == record.SlotTool {
			out = append(out, e.Name+":"+e.Failure)
		}
	}
	return out
}

func TestARunRecordsItsStartAndEnd(t *testing.T) {
	m := &scripted{steps: stepsOf(toolCall("search", `{"q":"x"}`), answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Name = "helper"
	sink := recorded(t, &a)
	out, rep, err := agent.Run(context.Background(), a, input("find x"))
	require.NoError(t, err)
	require.True(t, out.Cleared())

	runs := sink.runs(t)
	require.Len(t, runs, 1)
	run := runs[0]
	assert.NotEmpty(t, run.ID)
	assert.Equal(t, "helper", run.Agent)
	assert.Len(t, run.Calls, 2)
	require.NotNil(t, run.End)
	assert.Equal(t, record.End{Outcome: record.OutcomeCleared, Steps: rep.Steps, Tokens: rep.Tokens, Cost: rep.Cost}, *run.End)
	assert.Equal(t, 2, run.End.Steps)
	assert.Positive(t, run.End.Tokens)
	assert.Positive(t, run.End.Cost)
}

func TestARunNotClearedRecordsItsReason(t *testing.T) {
	m := &scripted{steps: stepsOf(changingCall())}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Limits = agent.DefaultLimits()
	a.Limits.MaxSteps = 2
	sink := recorded(t, &a)
	out, _, err := agent.Run(context.Background(), a, input("find x"))
	require.NoError(t, err)
	require.Equal(t, agent.ReasonStepLimit, out.Reason())

	runs := sink.runs(t)
	require.Len(t, runs, 1)
	require.NotNil(t, runs[0].End)
	assert.Equal(t, string(agent.ReasonStepLimit), runs[0].End.Outcome)
	assert.Equal(t, 2, runs[0].End.Steps)
}

// planted is error text that must never reach a recording.
const planted = "PLANTED-ERROR-TEXT-3d81"

// A tool call that gives the model no result is recorded with the tool's name
// and a fixed kind, never the error's text.
func TestAToolCallWithNoResultIsRecordedByKind(t *testing.T) {
	deny := func(hook.Event) (hook.Action, error) { return hook.Action{Deny: true}, nil }
	for name, tc := range map[string]struct {
		call  string
		setup func(t *testing.T, a *agent.Agent)
		want  string
	}{
		"unknown": {
			call: "lookup",
			setup: func(_ *testing.T, a *agent.Agent) {
				a.Tools = &tools{err: map[string]error{"lookup": fmt.Errorf("%w: %s", tool.ErrUnknown, planted)}}
			},
			want: record.ToolUnknown,
		},
		"invalid arguments": {
			call: "search",
			setup: func(_ *testing.T, a *agent.Agent) {
				a.Tools = &tools{err: map[string]error{"search": fmt.Errorf("%w: %s", tool.ErrInvalidArguments, planted)}}
			},
			want: record.ToolInvalid,
		},
		"failed": {
			call: "search",
			setup: func(_ *testing.T, a *agent.Agent) {
				a.Tools = &tools{err: map[string]error{"search": errors.New(planted)}}
			},
			want: record.ToolFailed,
		},
		"denied by a hook": {
			call: "search",
			setup: func(t *testing.T, a *agent.Agent) {
				withHooks(t, a, nil, map[hook.Point][]string{hook.BeforeTool: {"h"}}, map[string]hook.Hook{"h": &interceptor{f: deny}})
			},
			want: record.ToolDenied,
		},
		"rejected by an approver": {
			call: "search",
			setup: func(t *testing.T, a *agent.Agent) {
				a.Tools = &tools{out: map[string]string{"search": "found it"}, needs: map[string]bool{"search": true}}
				withHooks(t, a, nil, map[hook.Point][]string{hook.Approval: {"h"}}, map[string]hook.Hook{"h": answers(hook.Reject)})
			},
			want: record.ToolDenied,
		},
		"withheld by a hook": {
			call: "search",
			setup: func(t *testing.T, a *agent.Agent) {
				withHooks(t, a, nil, map[hook.Point][]string{hook.AfterTool: {"h"}}, map[string]hook.Hook{"h": &interceptor{f: deny}})
			},
			want: record.ToolWithheld,
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := &scripted{steps: stepsOf(toolCall(tc.call, `{"q":"x"}`), answer("done"))}
			a := newAgent(agent.Model{Name: "main", Chat: m})
			tc.setup(t, &a)
			sink := recorded(t, &a)
			out, _, err := agent.Run(context.Background(), a, input("find x"))
			require.NoError(t, err)
			require.True(t, out.Cleared(), "the run goes on")

			runs := sink.runs(t)
			require.Len(t, runs, 1)
			assert.Equal(t, []string{tc.call + ":" + tc.want}, toolEvents(runs[0]))
			assert.NotContains(t, sink.raw(), planted)
		})
	}
}

func TestAToolCallWithAResultRecordsNoToolEvent(t *testing.T) {
	m := &scripted{steps: stepsOf(toolCall("search", `{"q":"x"}`), answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	sink := recorded(t, &a)
	_, _, err := agent.Run(context.Background(), a, input("find x"))
	require.NoError(t, err)
	assert.Empty(t, toolEvents(sink.runs(t)[0]))
}

func TestAnOutputRetryIsRecorded(t *testing.T) {
	m := &scripted{steps: stepsOf(answer("not json"), answer(`{"cleared":true,"reason":"ok"}`))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	withOutput(t, &a, 1)
	sink := recorded(t, &a)
	out, _, err := agent.Run(context.Background(), a, input("check"))
	require.NoError(t, err)
	require.True(t, out.Cleared())

	var retries int
	for _, e := range sink.runs(t)[0].Events {
		if e.Slot == record.SlotOutput {
			assert.Equal(t, record.DecisionRetry, e.Decision)
			retries++
		}
	}
	assert.Equal(t, 1, retries)
}

// Runs sharing one recorder at the same time come apart when read, each with
// its own calls and end.
func TestRunsSharingARecorderComeApart(t *testing.T) {
	sink := newEntrySink(t)
	rec, err := record.NewRecorder(sink, secret.NewScrubber())
	require.NoError(t, err)
	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			steps := stepsOf(toolCall("search", `{"q":"x"}`), answer("done"))
			if i%2 == 1 {
				steps = stepsOf(answer("done"))
			}
			a := newAgent(agent.Model{Name: "main", Chat: &scripted{steps: steps}})
			a.Name = fmt.Sprintf("agent-%d", i)
			a.Recorder = rec
			_, _, err := agent.Run(context.Background(), a, input("find x"))
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	runs := sink.runs(t)
	require.Len(t, runs, n)
	ids := map[string]bool{}
	for _, run := range runs {
		ids[run.ID] = true
		require.NotNil(t, run.End, run.Agent)
		var i int
		_, err := fmt.Sscanf(run.Agent, "agent-%d", &i)
		require.NoError(t, err)
		want := 2
		if i%2 == 1 {
			want = 1
		}
		assert.Len(t, run.Calls, want, run.Agent)
		assert.Equal(t, want, run.End.Steps, run.Agent)
	}
	assert.Len(t, ids, n, "every run has its own ID")
	assert.NotContains(t, ids, "", "every entry was written inside a run")
}

// A version 1 recording written before runs were recorded still replays
// through the loop. The file was recorded on the main branch before version 2.
func TestAVersion1RecordingStillReplays(t *testing.T) {
	raw, err := os.Open("../record/testdata/v1.jsonl")
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()
	h, calls, err := record.Read(raw)
	require.NoError(t, err)
	require.Equal(t, 1, h.Version)
	require.Len(t, calls, 3)

	replay := record.NewReplay(h, calls, secret.NewScrubber())
	a := newAgent(agent.Model{Name: "main", Chat: replay.Model("main")})
	out, rep, err := agent.Run(context.Background(), a, input("when does the library open?"))
	require.NoError(t, err)
	answer, ok := out.Answer()
	require.True(t, ok, "%s", out)
	assert.Equal(t, "open from nine", answer)
	assert.Equal(t, 3, rep.Steps)
	assert.Zero(t, replay.Remaining())
}
