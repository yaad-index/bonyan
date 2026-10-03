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

func (d deaf) Hold(ctx context.Context, p approval.Pending) (<-chan approval.Decision, error) {
	if _, err := d.Store.Hold(ctx, p); err != nil {
		return nil, err
	}
	return make(chan approval.Decision), nil
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

func (a attached) Hold(ctx context.Context, p approval.Pending) (<-chan approval.Decision, error) {
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
	return newProcessWith(t, runs, approvals, []hook.Hook{answers(hook.Pending)}, steps...)
}

// newProcessWith is newProcess with the given hooks at the approval point,
// named a, b and on in order.
func newProcessWith(t *testing.T, runs runstore.Store, approvals approval.Store, hooks []hook.Hook, steps ...func(model.ChatRequest) (model.ChatResponse, error)) process {
	t.Helper()
	sink := &eventSink{}
	a, _, tl := gated(t, sink, hooks...)
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
			if err := store.Decide(context.Background(), aid, "a", approve); !errors.Is(err, approval.ErrUnknown) {
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
	out, rep, err := agent.Resume(context.Background(), second.agent, id)
	require.NoError(t, err)
	assert.Equal(t, agent.ReasonNotApproved, out.Reason())
	assert.Equal(t, 1, rep.Steps, "the step it was in")
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
	// The second process's action waits until the first has given up, so
	// the first meets the run claimed, not ended.
	release := make(chan struct{})
	second.agent.Tools = &blockingTools{tools: second.tools, release: release, started: make(chan struct{}, 4)}
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

	require.NoError(t, store.Decide(context.Background(), aid, "a", true))
	got := <-firstDone
	close(release)
	assert.Equal(t, agent.ReasonNotApproved, got.out.Reason())
	require.ErrorIs(t, got.rep.Err, agent.ErrRunClaimed)
	out := <-secondDone
	a, _ := out.Answer()
	assert.Equal(t, "second", a)
	assert.Empty(t, names(first.tools.calls)[1:], "the first process never ran the action")
	assert.Equal(t, []string{"search"}, names(second.tools.calls))
}

// Concurrent resumes of one run have one winner: while it waits on the
// action, every other is refused as resumed elsewhere.
func TestConcurrentResumesHaveOneWinner(t *testing.T) {
	runs, store := runstore.NewMemory(), approval.NewMemory()
	id, _, stop := suspended(t, runs, store, nil)
	defer stop()
	held := make(chan string, 8)
	results := make(chan error, 8)
	for range 8 {
		p := newProcess(t, runs, store, answer("done"))
		p.agent.Approvals = attached{store, held}
		go func() {
			_, _, err := agent.Resume(context.Background(), p.agent, id)
			results <- err
		}()
	}
	// The winner waits on the action until it is decided, so the seven
	// others return first.
	for range 7 {
		require.ErrorIs(t, <-results, agent.ErrRunClaimed)
	}
	require.NoError(t, store.Decide(context.Background(), <-held, "a", true))
	require.NoError(t, <-results, "the winner")
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
			require.NoError(t, store.Decide(context.Background(), held[0].ID, "a", approve))
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
		assert.NoError(t, store.Decide(context.Background(), held[0].ID, "a", true))
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

func (a approved) Hold(ctx context.Context, p approval.Pending) (<-chan approval.Decision, error) {
	if _, err := a.Store.Hold(ctx, p); err != nil {
		return nil, err
	}
	ch := make(chan approval.Decision, len(p.Approvers))
	for _, by := range p.Approvers {
		ch <- approval.Decision{By: by, Approve: true}
	}
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

// A call of the pending step still to run after the action runs from the
// saved state too, so one whose arguments hold a secret keeps the run from
// being saved, as the action's own would.
func TestALaterCallWithASecretIsNotSaved(t *testing.T) {
	res := secret.NewResolver(source{"key": "SECRET-9c4"})
	_, err := res.Scope("key").Resolve(context.Background(), "key")
	require.NoError(t, err)
	runs, store := runstore.NewMemory(), approval.NewMemory()
	threeCalls := func(model.ChatRequest) (model.ChatResponse, error) {
		return model.ChatResponse{
			ToolCalls: []model.ToolCall{
				{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{}`)},
				{ID: "c2", Name: "search", Arguments: json.RawMessage(`{"q":"x"}`)},
				{ID: "c3", Name: "lookup", Arguments: json.RawMessage(`{"k":"SECRET-9c4"}`)},
			},
			StopReason: model.StopToolCalls, Usage: usage,
		}, nil
	}
	p := newProcess(t, runs, store, threeCalls, answer("done"))
	p.agent.Scrubber = res.Scrubber()
	go decideWhenHeld(t, store, true)
	out, _, err := agent.Run(context.Background(), p.agent, input("find x"))
	require.NoError(t, err)
	assert.True(t, out.Cleared(), "the run goes on unsaved")
	assert.Contains(t, failures(p.sink, "runstore"), "save")
	list, err := runs.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, list)
	require.Len(t, p.tools.calls, 3)
	assert.JSONEq(t, `{"k":"SECRET-9c4"}`, string(p.tools.calls[2].Arguments), "run as asked")
}

// decideWhenHeld decides the one action store holds, once it holds one.
func decideWhenHeld(t *testing.T, store approval.Store, approve bool) {
	var held []approval.Pending
	if !assert.Eventually(t, func() bool {
		held, _ = store.List(context.Background())
		return len(held) == 1
	}, 2*time.Second, 2*time.Millisecond) {
		return
	}
	assert.NoError(t, store.Decide(context.Background(), held[0].ID, "a", approve))
}

// When a later action cannot be saved, the earlier action's state, marked
// proceeding, is removed rather than left stale: the later action waits
// unsaved and goes ahead when approved, and nothing claims it was resumed
// elsewhere.
func TestAFailedSecondSaveLeavesNoStaleState(t *testing.T) {
	res := secret.NewResolver(source{"key": "SECRET-9c4"})
	_, err := res.Scope("key").Resolve(context.Background(), "key")
	require.NoError(t, err)
	runs, store := runstore.NewMemory(), approval.NewMemory()
	p := newProcess(t, runs, store,
		toolCall("search", `{"q":"x"}`), toolCall("search", `{"q":"SECRET-9c4"}`), answer("done"))
	p.agent.Scrubber = res.Scrubber()
	go func() {
		decideWhenHeld(t, store, true)
		if assert.Eventually(t, func() bool {
			p.tools.mu.Lock()
			defer p.tools.mu.Unlock()
			return len(p.tools.calls) == 1
		}, 2*time.Second, 2*time.Millisecond) {
			decideWhenHeld(t, store, true)
		}
	}()
	out, rep, err := agent.Run(context.Background(), p.agent, input("find x"))
	require.NoError(t, err)
	require.True(t, out.Cleared(), "%s: %v", out, rep.Err)
	assert.Equal(t, []string{"search", "search"}, names(p.tools.calls))
	list, err := runs.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, list)
}

// proceeding is a run store whose Proceed finds the action marked already.
type proceeding struct{ runstore.Store }

func (proceeding) Proceed(context.Context, string, string) error { return runstore.ErrProceeding }

// An action found marked already is reported as such, not as resumed
// elsewhere.
func TestAnActionMarkedAlreadyIsReportedAsStarted(t *testing.T) {
	store := approval.NewMemory()
	p := newProcess(t, proceeding{runstore.NewMemory()}, store, toolCall("search", `{"q":"x"}`), answer("done"))
	go decideWhenHeld(t, store, true)
	out, rep, err := agent.Run(context.Background(), p.agent, input("find x"))
	require.NoError(t, err)
	assert.Equal(t, agent.ReasonNotApproved, out.Reason())
	require.ErrorIs(t, rep.Err, agent.ErrActionStarted)
	assert.Empty(t, p.tools.calls)
}

// A resume refused for its budget leaves the run for an agent with room.
func TestAResumeOverBudgetLeavesTheRun(t *testing.T) {
	runs, store := runstore.NewMemory(), approval.NewMemory()
	id, _, stop := suspended(t, runs, store, nil)
	defer stop()
	small := newProcess(t, runs, store, answer("done"))
	small.agent.Limits = agent.DefaultLimits()
	small.agent.Limits.Budget.MaxTokens = 10
	_, _, err := agent.Resume(context.Background(), small.agent, id)
	require.Error(t, err, "15 tokens spent before do not fit in 10")
	second := newProcess(t, runs, store, answer("done"))
	out, _, err := resume(t, second, id, store, true)
	require.NoError(t, err)
	assert.True(t, out.Cleared())
}

// gone is a run store whose Proceed finds the run removed.
type gone struct{ runstore.Store }

func (gone) Proceed(context.Context, string, string) error { return runstore.ErrUnknown }

// A run whose saved state was removed while it waited, by a resume that ran
// it to its end or by deleting the subject, gives its action up as unknown.
func TestARemovedRunGivesItsActionUp(t *testing.T) {
	store := approval.NewMemory()
	p := newProcess(t, gone{runstore.NewMemory()}, store, toolCall("search", `{"q":"x"}`), answer("done"))
	go decideWhenHeld(t, store, true)
	out, rep, err := agent.Run(context.Background(), p.agent, input("find x"))
	require.NoError(t, err)
	assert.Equal(t, agent.ReasonNotApproved, out.Reason())
	require.ErrorIs(t, rep.Err, agent.ErrUnknownRun)
	assert.Empty(t, p.tools.calls)
}

// A run resumed after a restart keeps the decisions made on its action
// while it was down: it waits only for the approvers that had not decided,
// and a denial made while it was down ends the wait at once (ADR 0001 §12).
func TestAResumedRunKeepsTheDecisionsMade(t *testing.T) {
	for _, c := range []struct {
		name     string
		down     approval.Decision
		after    []approval.Decision
		runs     bool
		recorded []string
	}{
		{"approved while down", approval.Decision{By: "b", Approve: true}, []approval.Decision{{By: "a", Approve: true}}, true, []string{"b:approved", "a:approved"}},
		{"denied while down", approval.Decision{By: "a"}, nil, false, []string{"a:denied"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			runs, store := runstore.NewMemory(), approval.NewMemory()
			pendingTwice := []hook.Hook{answers(hook.Pending), answers(hook.Pending)}
			first := newProcessWith(t, &stopping{Store: runs}, deaf{store}, pendingTwice, twoCalls, answer("never"))
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _, _ = agent.Run(ctx, first.agent, input("find x"))
			}()
			defer func() { cancel(); <-done }()
			var held []approval.Pending
			require.Eventually(t, func() bool {
				list, _ := runs.List(context.Background())
				held, _ = store.List(context.Background())
				return len(list) == 1 && len(held) == 1
			}, 2*time.Second, 5*time.Millisecond)
			list, err := runs.List(context.Background())
			require.NoError(t, err)
			assert.Equal(t, []string{"a", "b"}, held[0].Approvers)
			require.NoError(t, store.Decide(context.Background(), held[0].ID, c.down.By, c.down.Approve))

			second := newProcessWith(t, runs, store, pendingTwice, answer("done"))
			seen := make(chan string, 1)
			second.agent.Approvals = attached{store, seen}
			go func() {
				aid := <-seen
				for _, d := range c.after {
					assert.NoError(t, store.Decide(context.Background(), aid, d.By, d.Approve))
				}
			}()
			out, _, err := agent.Resume(context.Background(), second.agent, list[0].Run)
			require.NoError(t, err)
			assert.True(t, out.Cleared(), out.String())
			if c.runs {
				assert.Equal(t, []string{"search"}, names(second.tools.calls))
			} else {
				assert.Empty(t, second.tools.calls)
			}
			assert.Equal(t, c.recorded, approvals(second.sink))
		})
	}
}

// failingHold is an approval store that cannot hold an action.
type failingHold struct{ approval.Store }

func (failingHold) Hold(context.Context, approval.Pending) (<-chan approval.Decision, error) {
	return nil, errors.New("disk full")
}

// callSink keeps the chat calls recorded.
type callSink struct {
	eventSink
	calls []record.Call
}

func (s *callSink) Write(e record.Entry) error {
	s.mu.Lock()
	if e.Call != nil {
		s.calls = append(s.calls, *e.Call)
	}
	s.mu.Unlock()
	return s.eventSink.Write(e)
}

// A resumed run's calls mark its input, the message it started from, which
// its restored turns begin with.
func TestAResumedRunMarksItsInput(t *testing.T) {
	runs, store := runstore.NewMemory(), approval.NewMemory()
	id, _, stop := suspended(t, runs, store, nil)
	defer stop()
	second := newProcess(t, runs, store, answer("done"))
	sink := &callSink{}
	rec, err := record.NewRecorder(sink, secret.NewScrubber())
	require.NoError(t, err)
	second.agent.Recorder = rec
	out, _, err := resume(t, second, id, store, true)
	require.NoError(t, err)
	require.True(t, out.Cleared(), out.String())
	require.NotEmpty(t, sink.calls)
	for _, c := range sink.calls {
		var marked []record.Part
		for _, m := range c.Request.Messages {
			for _, p := range m.Parts {
				if p.Input {
					marked = append(marked, p)
				}
			}
		}
		require.Len(t, marked, 1)
		assert.Equal(t, agent.SectionUserMessage, marked[0].Section)
		require.Len(t, marked[0].Items, 1)
		assert.Equal(t, "find x", marked[0].Items[0].Text)
	}
}
