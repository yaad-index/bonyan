package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/approval"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/hook"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/runstore"
	"github.com/yaad-index/bonyan/secret"
)

// deaf holds actions in the shared store but never hears their decision, and
// drops nothing, as a process that stopped while it waited.
type deaf struct{ approval.Store }

func (d deaf) Hold(ctx context.Context, p approval.Pending) (<-chan bool, error) {
	if _, err := d.Store.Hold(ctx, p); err != nil {
		return nil, err
	}
	return make(chan bool), nil
}

func (deaf) Drop(context.Context, string) error { return nil }

// stopping is a process's view of the run store that goes dead once its run
// is saved, as a process that stopped right after saving: nothing it does
// later reaches the store.
type stopping struct {
	runstore.Store
	mu      sync.Mutex
	stopped bool
}

func (s *stopping) Save(ctx context.Context, st runstore.State, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil
	}
	s.stopped = true
	return s.Store.Save(ctx, st, token)
}

func (s *stopping) Proceed(context.Context, string, string) error { return nil }
func (s *stopping) Delete(context.Context, string, string) error  { return nil }

// attached tells when a resumed run holds its action again.
type attached struct {
	approval.Store
	held chan string
}

func (a attached) Hold(ctx context.Context, p approval.Pending) (<-chan bool, error) {
	ch, err := a.Store.Hold(ctx, p)
	a.held <- p.ID
	return ch, err
}

// twoCalls asks for lookup, then for search, in one step.
func twoCalls(req model.ChatRequest) (model.ChatResponse, error) {
	return model.ChatResponse{
		ToolCalls: []model.ToolCall{
			{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{}`)},
			{ID: "c2", Name: "search", Arguments: json.RawMessage(`{"q":"x"}`)},
		},
		StopReason: model.StopToolCalls, Usage: usage,
	}, nil
}

// process is one process running or resuming the gated agent.
type process struct {
	agent agent.Agent
	model *scripted
	tools *tools
	sink  *eventSink
}

// newProcess is the gated agent of a process: search needs approval, which
// the approver says is pending, and lookup runs at once.
func newProcess(t *testing.T, runs runstore.Store, approvals approval.Store, steps ...func(model.ChatRequest) (model.ChatResponse, error)) process {
	t.Helper()
	sink := &eventSink{}
	a, _, tl := gated(t, sink, answers(hook.Pending))
	m := &scripted{steps: stepsOf(steps...)}
	a.Models = []agent.Model{{Name: "main", Chat: m}}
	tl.out["lookup"] = "LOOKED-UP-9c4"
	a.Approvals, a.RunStore, a.Subject = approvals, runs, "ana"
	rec, err := record.NewRecorder(sink, secret.NewScrubber())
	require.NoError(t, err)
	a.Recorder = rec
	return process{agent: a, model: m, tools: tl, sink: sink}
}

// suspended starts a run in a process that stops while its action waits, and
// returns the run's id. stop ends that process's goroutine.
func suspended(t *testing.T, runs runstore.Store, store approval.Store, change func(*agent.Agent)) (id string, first process, stop func()) {
	t.Helper()
	return suspendedOn(t, runs, store, "find x", change)
}

// suspendedOn is suspended with the user's message msg.
func suspendedOn(t *testing.T, runs runstore.Store, store approval.Store, msg string, change func(*agent.Agent)) (id string, first process, stop func()) {
	t.Helper()
	first = newProcess(t, &stopping{Store: runs}, deaf{store}, twoCalls, answer("never"))
	if change != nil {
		change(&first.agent)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = agent.Run(ctx, first.agent, input(msg))
	}()
	require.Eventually(t, func() bool {
		list, _ := runs.List(context.Background())
		return len(list) == 1
	}, 2*time.Second, 5*time.Millisecond)
	list, err := runs.List(context.Background())
	require.NoError(t, err)
	return list[0].Run, first, func() { cancel(); <-done }
}

// resume resumes id in a new process and decides its action once it is held
// again.
func resume(t *testing.T, p process, id string, store approval.Store, approve bool) (agent.Outcome, agent.Report, error) {
	t.Helper()
	held := make(chan string, 1)
	p.agent.Approvals = attached{store, held}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		select {
		case aid := <-held:
			// A run whose time is up has dropped its action already.
			if err := store.Decide(context.Background(), aid, approve); !errors.Is(err, approval.ErrUnknown) {
				assert.NoError(t, err)
			}
		case <-stop:
		}
	}()
	out, rep, err := agent.Resume(context.Background(), p.agent, id)
	close(stop)
	<-done
	return out, rep, err
}

// A run stopped while its action waited is resumed by a new process: the
// calls done before the action are not run again, the action runs once when
// approved, and the run goes on to its answer, spending on top of what it
// spent before. Its saved state is gone when it ends.
func TestARunResumesAfterARestart(t *testing.T) {
	runs, store := runstore.NewMemory(), approval.NewMemory()
	id, first, stop := suspended(t, runs, store, nil)
	defer stop()
	assert.Equal(t, []string{"lookup"}, names(first.tools.calls), "lookup ran before the pending action")

	second := newProcess(t, runs, store, answer("done"))
	starts := &startSink{}
	rec, err := record.NewRecorder(starts, secret.NewScrubber())
	require.NoError(t, err)
	second.agent.Recorder = rec
	out, rep, err := resume(t, second, id, store, true)
	require.NoError(t, err)
	assert.Equal(t, []string{id + " resumed"}, starts.started(), "the run keeps its id, marked resumed")
	a, ok := out.Answer()
	require.True(t, ok, out.String())
	assert.Equal(t, "done", a)
	assert.Equal(t, []string{"search"}, names(second.tools.calls), "only the pending action, once")
	assert.Equal(t, []string{"lookup"}, names(first.tools.calls))
	assert.Equal(t, int64(30), rep.Tokens, "what both processes spent")
	assert.Equal(t, 2, rep.Steps, "the run goes on from its second step")
	assert.Equal(t, []string{"a:approved"}, approvals(second.sink))

	require.Len(t, second.model.reqs, 1)
	msgs := second.model.reqs[0].Messages
	assert.Equal(t, "find x", itemText(t, msgs[1]), "the run's message")
	assert.Equal(t, "LOOKED-UP-9c4", itemText(t, msgs[3]), "lookup's saved result")
	assert.Equal(t, "found it", itemText(t, msgs[4]), "the action's result")
	list, err := runs.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, list, "removed when the run ended")
}

// startSink keeps the run of each start written, and whether it resumed.
type startSink struct {
	mu   sync.Mutex
	runs []string
}

func (s *startSink) Write(e record.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Start != nil {
		r := e.Run
		if e.Start.Resumed {
			r += " resumed"
		}
		s.runs = append(s.runs, r)
	}
	return nil
}
func (*startSink) Full() bool      { return false }
func (*startSink) Subject() string { return "" }
func (*startSink) Close() error    { return nil }

func (s *startSink) started() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.runs...)
}

func names(calls []model.ToolCall) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c.Name)
	}
	return out
}

func itemText(t *testing.T, m model.Message) string {
	t.Helper()
	require.NotEmpty(t, m.Parts)
	it, ok := item(m.Parts[0])
	require.True(t, ok, "%T", m.Parts[0])
	return it.Raw()
}

// A denied action is reported as denied and the run goes on; an action whose
// approval timed out while no process waited is cancelled.
func TestAResumedActionIsDeniedOrTimesOut(t *testing.T) {
	runs, store := runstore.NewMemory(), approval.NewMemory()
	id, _, stop := suspended(t, runs, store, nil)
	defer stop()
	second := newProcess(t, runs, store, answer("done"))
	out, _, err := resume(t, second, id, store, false)
	require.NoError(t, err)
	assert.True(t, out.Cleared())
	assert.Empty(t, second.tools.calls)
	assert.Equal(t, []string{"a:denied"}, approvals(second.sink))

	runs, store = runstore.NewMemory(), approval.NewMemory()
	id, _, stop2 := suspended(t, runs, store, func(a *agent.Agent) {
		a.Limits = agent.DefaultLimits()
		a.Limits.ApprovalTimeout = 50 * time.Millisecond
	})
	defer stop2()
	time.Sleep(60 * time.Millisecond)
	second = newProcess(t, runs, store, answer("done"))
	second.agent.Approvals = store
	out, _, err = agent.Resume(context.Background(), second.agent, id)
	require.NoError(t, err)
	assert.Equal(t, agent.ReasonNotApproved, out.Reason())
	assert.Empty(t, second.tools.calls)
	assert.Equal(t, []string{"a:timed out"}, approvals(second.sink))
}

// Only one process takes a run: a second Resume is refused while the first
// holds it, and the process that ran it from the start, still waiting, gives
// its action up when the decision comes, so the action runs once.
func TestOnlyOneProcessTakesARun(t *testing.T) {
	runs, store := runstore.NewMemory(), approval.NewMemory()
	first := newProcess(t, runs, store, twoCalls, answer("first"))
	type result struct {
		out agent.Outcome
		rep agent.Report
	}
	firstDone := make(chan result, 1)
	go func() {
		out, rep, _ := agent.Run(context.Background(), first.agent, input("find x"))
		firstDone <- result{out, rep}
	}()
	require.Eventually(t, func() bool {
		list, _ := runs.List(context.Background())
		return len(list) == 1
	}, 2*time.Second, 5*time.Millisecond)
	list, err := runs.List(context.Background())
	require.NoError(t, err)
	id := list[0].Run

	second := newProcess(t, runs, store, answer("second"))
	held := make(chan string, 1)
	second.agent.Approvals = attached{store, held}
	secondDone := make(chan agent.Outcome, 1)
	go func() {
		out, _, err := agent.Resume(context.Background(), second.agent, id)
		assert.NoError(t, err)
		secondDone <- out
	}()
	aid := <-held

	third := newProcess(t, runs, store, answer("third"))
	_, _, err = agent.Resume(context.Background(), third.agent, id)
	require.ErrorIs(t, err, agent.ErrRunClaimed)

	require.NoError(t, store.Decide(context.Background(), aid, true))
	got := <-firstDone
	assert.Equal(t, agent.ReasonNotApproved, got.out.Reason())
	require.ErrorIs(t, got.rep.Err, agent.ErrRunClaimed)
	out := <-secondDone
	a, _ := out.Answer()
	assert.Equal(t, "second", a)
	assert.Empty(t, names(first.tools.calls)[1:], "the first process never ran the action")
	assert.Equal(t, []string{"search"}, names(second.tools.calls))
}

// Concurrent resumes of one run have one winner.
func TestConcurrentResumesHaveOneWinner(t *testing.T) {
	runs, store := runstore.NewMemory(), approval.NewMemory()
	id, _, stop := suspended(t, runs, store, nil)
	defer stop()
	var mu sync.Mutex
	var claimed, won int
	var wg sync.WaitGroup
	held := make(chan string, 8)
	for range 8 {
		p := newProcess(t, runs, store, answer("done"))
		p.agent.Approvals = attached{store, held}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := agent.Resume(context.Background(), p.agent, id)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case assert.ErrorIs(t, err, agent.ErrRunClaimed):
				claimed++
			}
		}()
	}
	require.NoError(t, store.Decide(context.Background(), <-held, true))
	wg.Wait()
	assert.Equal(t, 1, won)
	assert.Equal(t, 7, claimed)
}

// Once the action is decided and going ahead, the run cannot be resumed, so a
// stop while the action runs never leads to running it again. A denied action
// goes ahead the same way, with the calls after it.
func TestADecidedActionIsNeverResumed(t *testing.T) {
	for _, approve := range []bool{true, false} {
		t.Run(map[bool]string{true: "approved", false: "denied"}[approve], func(t *testing.T) {
			runs, store := runstore.NewMemory(), approval.NewMemory()
			first := newProcess(t, runs, store, twoCalls, answer("done"))
			release := make(chan struct{})
			blocking := &blockingTools{tools: first.tools, release: release, started: make(chan struct{}, 4)}
			first.agent.Tools = blocking
			if !approve {
				// A denied action goes on to the next step, whose model call
				// stands for the process stopping.
				first.model.steps = stepsOf(twoCalls, func(model.ChatRequest) (model.ChatResponse, error) {
					blocking.started <- struct{}{}
					<-release
					return answer("done")(model.ChatRequest{})
				})
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _, _ = agent.Run(context.Background(), first.agent, input("find x"))
			}()
			var id string
			require.Eventually(t, func() bool {
				held, _ := store.List(context.Background())
				list, _ := runs.List(context.Background())
				if len(held) != 1 || len(list) != 1 {
					return false
				}
				id = list[0].Run
				return true
			}, 2*time.Second, 5*time.Millisecond)
			held, err := store.List(context.Background())
			require.NoError(t, err)
			<-blocking.started // lookup
			require.NoError(t, store.Decide(context.Background(), held[0].ID, approve))
			<-blocking.started // the action, or the step after the denial

			second := newProcess(t, runs, store, answer("done"))
			_, _, err = agent.Resume(context.Background(), second.agent, id)
			require.ErrorIs(t, err, agent.ErrActionStarted)
			close(release)
			<-done
			assert.Empty(t, second.tools.calls)
		})
	}
}

// blockingTools signals each call it starts, and holds search until released.
type blockingTools struct {
	*tools
	release chan struct{}
	started chan struct{}
}

func (b *blockingTools) Call(ctx context.Context, tc model.ToolCall) (string, error) {
	b.started <- struct{}{}
	if tc.Name == "search" {
		<-b.release
	}
	return b.tools.Call(ctx, tc)
}

// A run the store does not hold is an error, never a fresh run.
func TestResumingAnUnknownRunIsAnError(t *testing.T) {
	p := newProcess(t, runstore.NewMemory(), approval.NewMemory(), answer("done"))
	_, _, err := agent.Resume(context.Background(), p.agent, "nothing")
	require.ErrorIs(t, err, agent.ErrUnknownRun)
	assert.Empty(t, p.model.reqs)

	p.agent.RunStore = nil
	_, _, err = agent.Resume(context.Background(), p.agent, "nothing")
	require.Error(t, err, "no run store")
	p.agent.RunStore, p.agent.Subject = runstore.NewMemory(), ""
	_, _, err = agent.Run(context.Background(), p.agent, input("x"))
	require.Error(t, err, "a run store needs a subject")
}

// A run is resumed only by the agent it started as: other instructions, tool
// definitions, material or history are refused, and leave the run for the
// agent it started as.
func TestAChangedAgentCannotResume(t *testing.T) {
	runs, store := runstore.NewMemory(), approval.NewMemory()
	id, _, stop := suspended(t, runs, store, nil)
	defer stop()
	for name, change := range map[string]func(*agent.Agent){
		"instructions": func(a *agent.Agent) { a.Instructions = content.Instruction("answer at length") },
		"tools":        func(a *agent.Agent) { a.Tools = &otherDefs{a.Tools.(*tools)} },
		"material": func(a *agent.Agent) {
			a.Material = []content.Untrusted{content.From(content.Provenance{Kind: content.KindFetched}, "a page")}
		},
		"history": func(a *agent.Agent) { a.History = []model.Message{earlierTurn("m0", "before")} },
	} {
		p := newProcess(t, runs, store, answer("done"))
		change(&p.agent)
		_, _, err := agent.Resume(context.Background(), p.agent, id)
		require.ErrorIs(t, err, agent.ErrChanged, name)
	}
	p := newProcess(t, runs, store, answer("done"))
	out, _, err := resume(t, p, id, store, true)
	require.NoError(t, err)
	assert.True(t, out.Cleared(), "the agent it started as still can")
}

// otherDefs describes its tools differently.
type otherDefs struct{ *tools }

func (o *otherDefs) Definitions() []model.ToolDef {
	return []model.ToolDef{{Name: "search", Description: "now described", Parameters: json.RawMessage(`{"type":"object"}`)}}
}

// What is saved holds no resolved secret, so a resumed run sees the
// placeholder where one was; and a pending action whose arguments hold one is
// not saved, since the saved action would not be the one approved.
func TestASavedRunHoldsNoSecret(t *testing.T) {
	res := secret.NewResolver(source{"key": "SECRET-9c4"})
	_, err := res.Scope("key").Resolve(context.Background(), "key")
	require.NoError(t, err)
	runs, store := runstore.NewMemory(), approval.NewMemory()
	id, _, stop := suspended(t, runs, store, func(a *agent.Agent) {
		a.Scrubber = res.Scrubber()
		a.Tools.(*tools).out["lookup"] = "LOOKED-UP SECRET-9c4"
	})
	defer stop()
	list, err := runs.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.NotContains(t, string(list[0].Data), "SECRET-9c4")
	assert.Contains(t, string(list[0].Data), "LOOKED-UP")

	second := newProcess(t, runs, store, answer("done"))
	second.agent.Scrubber = res.Scrubber()
	_, _, err = resume(t, second, id, store, true)
	require.NoError(t, err)
	got := itemText(t, second.model.reqs[0].Messages[3])
	assert.True(t, strings.HasPrefix(got, "LOOKED-UP "), got)
	assert.NotContains(t, got, "SECRET-9c4")

	runs = runstore.NewMemory()
	p := newProcess(t, runs, store, toolCall("search", `{"q":"SECRET-9c4"}`), answer("done"))
	p.agent.Scrubber = res.Scrubber()
	sink := p.sink
	go func() {
		require.Eventually(t, func() bool {
			held, _ := store.List(context.Background())
			return len(held) == 1
		}, 2*time.Second, 5*time.Millisecond)
		held, _ := store.List(context.Background())
		assert.NoError(t, store.Decide(context.Background(), held[0].ID, true))
	}()
	out, _, err := agent.Run(context.Background(), p.agent, input("find x"))
	require.NoError(t, err)
	assert.True(t, out.Cleared(), "the run goes on unsaved")
	assert.Contains(t, failures(sink, "runstore"), "save")
	list, err = runs.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, list)
}

// failures are the names of the failure events recorded in slot.
func failures(sink *eventSink, slot string) []string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var out []string
	for _, e := range sink.events {
		if e.Slot == slot && e.Failure != "" {
			out = append(out, e.Name)
		}
	}
	return out
}

// A saved part is classified again under the policy as it is when the run is
// resumed.
func TestASavedPartIsClassifiedAgain(t *testing.T) {
	runs, store := runstore.NewMemory(), approval.NewMemory()
	id, _, stop := suspended(t, runs, store, func(a *agent.Agent) { a.Trust = trustKind{kind: content.KindTool} })
	defer stop()
	for _, tc := range []struct {
		policy  *trustKind
		trusted bool
	}{{nil, false}, {&trustKind{kind: content.KindTool}, true}} {
		second := newProcess(t, runs, store, answer("done"))
		if tc.policy != nil {
			second.agent.Trust = *tc.policy
		}
		_, _, err := resume(t, second, id, store, false)
		require.NoError(t, err)
		lookup := second.model.reqs[0].Messages[3]
		assert.Equal(t, tc.trusted, lookup.Parts[0].Trusted())
		// Resumed once; the next resume needs the run suspended again.
		id, _, stop = suspended(t, runs, store, func(a *agent.Agent) { a.Trust = trustKind{kind: content.KindTool} })
		defer stop()
	}
}

// Memory is not saved: a resumed run recalls it again, and stores the user's
// message only once.
func TestAResumedRunRecallsMemoryAgain(t *testing.T) {
	runs, store := runstore.NewMemory(), approval.NewMemory()
	mem := memoryStore(t, "MEMORY-9c4 find things")
	id, _, stop := suspended(t, runs, store, func(a *agent.Agent) { a.Memory, a.Session = mem, "s1" })
	defer stop()
	list, err := runs.List(context.Background())
	require.NoError(t, err)
	assert.NotContains(t, string(list[0].Data), "MEMORY-9c4")

	second := newProcess(t, runs, store, answer("done"))
	second.agent.Memory, second.agent.Session = mem, "s1"
	_, _, err = resume(t, second, id, store, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"MEMORY-9c4 find things"}, recalledIn(second.model.reqs[0]))
	history, err := mem.History(context.Background(), "ana", "s1")
	require.NoError(t, err)
	var texts []string
	for _, h := range history {
		texts = append(texts, h.(content.Untrusted).Raw())
	}
	assert.Equal(t, []string{"find x", "done"}, texts, "the message once, then the answer")
}

// A resumed run keeps the deadline it started with: one that passed while no
// process ran it ends the run, and its action never runs.
// The decision may be in already, as a durable store keeps one made while no
// process waited: a time past still wins, every time.
func TestAResumedRunKeepsItsDeadline(t *testing.T) {
	limits := agent.DefaultLimits()
	limits.Deadline = 50 * time.Millisecond
	for range 20 {
		runs, store := runstore.NewMemory(), approval.NewMemory()
		id, _, stop := suspended(t, runs, store, func(a *agent.Agent) { a.Limits = limits })
		time.Sleep(60 * time.Millisecond)
		second := newProcess(t, runs, store, answer("done"))
		second.agent.Limits = limits
		second.agent.Approvals = approved{store}
		out, _, err := agent.Resume(context.Background(), second.agent, id)
		stop()
		require.NoError(t, err)
		require.False(t, out.Cleared(), out.String())
		require.Empty(t, second.tools.calls)
	}
}

// approved holds an action whose approval is in already.
type approved struct{ approval.Store }

func (a approved) Hold(ctx context.Context, p approval.Pending) (<-chan bool, error) {
	if _, err := a.Store.Hold(ctx, p); err != nil {
		return nil, err
	}
	ch := make(chan bool, 1)
	ch <- true
	return ch, nil
}

// Loop counts carry over: a call repeated after the resume counts the times
// it was made before.
func TestLoopCountsCarryOver(t *testing.T) {
	runs, store := runstore.NewMemory(), approval.NewMemory()
	id, _, stop := suspended(t, runs, store, func(a *agent.Agent) { a.LoopThreshold = 2 })
	defer stop()
	second := newProcess(t, runs, store, func(model.ChatRequest) (model.ChatResponse, error) {
		return model.ChatResponse{ToolCalls: []model.ToolCall{{ID: "c9", Name: "lookup", Arguments: json.RawMessage(`{}`)}}, StopReason: model.StopToolCalls, Usage: usage}, nil
	})
	second.agent.LoopThreshold = 2
	out, _, err := resume(t, second, id, store, true)
	require.NoError(t, err)
	assert.Equal(t, agent.ReasonLoopDetected, out.Reason(), "lookup ran once before the restart")
}

// The output retries spent carry over, and bonyan's own text in the saved
// turns comes back as bonyan's.
func TestRetriesCarryOver(t *testing.T) {
	out1, err := agent.OutputFor[struct {
		A string `json:"a"`
	}](1)
	require.NoError(t, err)
	runs, store := runstore.NewMemory(), approval.NewMemory()
	id, _, stop := suspended(t, runs, store, func(a *agent.Agent) {
		a.Output = out1
		a.Models[0].Chat.(*scripted).steps = stepsOf(answer("not json"), twoCalls)
	})
	defer stop()
	second := newProcess(t, runs, store, answer("still not json"), answer(`{"a":"x"}`))
	second.agent.Output = out1
	out, _, err := resume(t, second, id, store, true)
	require.NoError(t, err)
	assert.Equal(t, agent.ReasonInvalidOutput, out.Reason(), "the one retry was spent before the restart")
	var retry []string
	for _, m := range second.model.reqs[0].Messages {
		for _, p := range m.Parts {
			if tr, ok := p.(content.Trusted); ok && tr.Provenance() == (content.Provenance{}) && strings.HasPrefix(tr.String(), "Your answer did not match") {
				retry = append(retry, tr.String())
			}
		}
	}
	assert.Len(t, retry, 1, "bonyan's retry message, as bonyan's")
}

// The user's message is saved scrubbed too.
func TestASavedMessageHoldsNoSecret(t *testing.T) {
	res := secret.NewResolver(source{"key": "SECRET-9c4"})
	_, err := res.Scope("key").Resolve(context.Background(), "key")
	require.NoError(t, err)
	runs, store := runstore.NewMemory(), approval.NewMemory()
	_, _, stop := suspendedOn(t, runs, store, "find SECRET-9c4", func(a *agent.Agent) { a.Scrubber = res.Scrubber() })
	defer stop()
	list, err := runs.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.NotContains(t, string(list[0].Data), "SECRET-9c4")
}
