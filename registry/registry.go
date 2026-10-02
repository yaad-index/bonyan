// Package registry assembles an agent's parts from configuration.
//
// Every pluggable slot holds named implementations. A program registers its own
// under new names, and configuration picks one per slot by name, so swapping an
// implementation is a configuration change (ADR 0001, "Everything is pluggable").
//
// A registry is an instance, not a package-level global: nothing registers
// itself by being imported, and two agents in one program can hold different
// sets. Assemble is the only way to obtain a configured part, and it hands out
// each part inside bonyan's wrapper for its slot. The invariants of ADR 0001 are
// applied in those wrappers, around the implementations, never inside them.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/eval/score"
	"github.com/yaad-index/bonyan/hook"
	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/memory/inmem"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/secret"
	"github.com/yaad-index/bonyan/trust"
)

// Factory builds an implementation from its slot's options, which are passed
// through from configuration unparsed.
type Factory[T any] func(options json.RawMessage) (T, error)

// The slot names used in errors.
const (
	SlotChat       = "chat"
	SlotEmbedder   = "embedder"
	SlotClassifier = "classifier"
	SlotTrust      = "trust"
	SlotHook       = "hook"
	SlotSecret     = "secret"
	SlotRecording  = "recording"
	SlotMemory     = "memory"
	SlotEvaluator  = "evaluator"
	SlotQueue      = "eval_queue"
)

// QueueInMem is the name the in-process evaluation queue is registered under.
// Its options are {"retention": "<duration>"}, required.
const QueueInMem = "inmem"

// MemoryInMem is the name the in-process memory backend is registered under.
const MemoryInMem = "inmem"

// The names the built-in secret sources are registered under. "dir" takes the
// option {"path": "<directory>"}.
const (
	SecretEnv = "env"
	SecretDir = "dir"
)

// ErrDuplicate reports a name already registered in a slot.
var ErrDuplicate = errors.New("registry: name already registered")

// ErrUnknown reports configuration naming an implementation that is not
// registered.
var ErrUnknown = errors.New("registry: unknown implementation")

// ErrNoRecorder reports configuration whose parts must be recorded, assembled
// without a recording: a trust policy other than the default, whose decisions
// may declare sources trusted, or hooks, whose changes, denials and failures are
// recorded with the hook's name.
var ErrNoRecorder = errors.New("registry: a recording is required")

// ErrHookPoint is what Assemble returns for a hook that may change or deny
// attached at a point that allows neither, which could not act as written.
var ErrHookPoint = errors.New("registry: hook cannot act at its point")

// slot holds the named factories for one kind of part.
type slot[T any] struct {
	name      string
	factories map[string]Factory[T]
}

func newSlot[T any](name string) *slot[T] {
	return &slot[T]{name: name, factories: map[string]Factory[T]{}}
}

func (s *slot[T]) register(name string, f Factory[T]) error {
	if name == "" {
		return fmt.Errorf("registry: %s: empty implementation name", s.name)
	}
	if f == nil {
		return fmt.Errorf("registry: %s %q: nil factory", s.name, name)
	}
	if _, ok := s.factories[name]; ok {
		return fmt.Errorf("%w: %s %q", ErrDuplicate, s.name, name)
	}
	s.factories[name] = f
	return nil
}

func (s *slot[T]) names() []string {
	out := make([]string, 0, len(s.factories))
	for n := range s.factories {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// build constructs the configured implementation, reporting a missing or failing
// one with the slot and the names that are registered.
func (s *slot[T]) build(cfg SlotConfig) (T, error) {
	var zero T
	f, ok := s.factories[cfg.Impl]
	if !ok {
		return zero, fmt.Errorf("%w: %s %q (registered: %v)", ErrUnknown, s.name, cfg.Impl, s.names())
	}
	impl, err := f(cfg.Options)
	if err != nil {
		return zero, fmt.Errorf("registry: build %s %q: %w", s.name, cfg.Impl, err)
	}
	if isNil(impl) {
		return zero, fmt.Errorf("registry: build %s %q: factory returned no implementation", s.name, cfg.Impl)
	}
	return impl, nil
}

// isNil reports whether v is nil or holds a nil pointer, func, map, channel or
// slice, none of which a wrapper can call.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Map, reflect.Chan, reflect.Slice, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

// Registry holds the named implementations of every slot.
type Registry struct {
	mu          sync.RWMutex
	chat        *slot[model.Chat]
	embedders   *slot[model.Embedder]
	classifiers *slot[model.Classifier]
	policies    *slot[trust.Policy]
	hooks       *slot[hook.Hook]
	secrets     *slot[secret.Source]
	sinks       *slot[record.Sink]
	memories    *slot[memory.Backend]
	evaluators  *slot[score.Evaluator]
	queues      *slot[record.Queue]
}

// New returns a registry holding only the built-ins: the default trust policy,
// under trust.DefaultName, the environment and directory secret sources, the
// file recording sink and the in-process memory backend.
func New() *Registry {
	r := &Registry{
		chat:        newSlot[model.Chat](SlotChat),
		embedders:   newSlot[model.Embedder](SlotEmbedder),
		classifiers: newSlot[model.Classifier](SlotClassifier),
		policies:    newSlot[trust.Policy](SlotTrust),
		hooks:       newSlot[hook.Hook](SlotHook),
		secrets:     newSlot[secret.Source](SlotSecret),
		sinks:       newSlot[record.Sink](SlotRecording),
		memories:    newSlot[memory.Backend](SlotMemory),
		evaluators:  newSlot[score.Evaluator](SlotEvaluator),
		queues:      newSlot[record.Queue](SlotQueue),
	}
	r.queues.factories[QueueInMem] = memQueue
	r.policies.factories[trust.DefaultName] = func(json.RawMessage) (trust.Policy, error) {
		return trust.Default{}, nil
	}
	r.secrets.factories[SecretEnv] = func(json.RawMessage) (secret.Source, error) {
		return secret.Env{}, nil
	}
	r.secrets.factories[SecretDir] = dirSource
	r.sinks.factories[SinkFile] = fileSink
	r.memories.factories[MemoryInMem] = func(json.RawMessage) (memory.Backend, error) {
		return inmem.New(), nil
	}
	return r
}

func dirSource(options json.RawMessage) (secret.Source, error) {
	var o struct {
		Path string `json:"path"`
	}
	if len(options) > 0 {
		if err := json.Unmarshal(options, &o); err != nil {
			return nil, err
		}
	}
	if o.Path == "" {
		return nil, errors.New(`options need a "path"`)
	}
	return secret.Dir{Path: o.Path}, nil
}

// RegisterChat registers a chat model implementation under name. Registering a
// name twice is an error: an implementation is never replaced silently.
func (r *Registry) RegisterChat(name string, f Factory[model.Chat]) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.chat.register(name, f)
}

// RegisterEmbedder registers an embedding model implementation under name.
func (r *Registry) RegisterEmbedder(name string, f Factory[model.Embedder]) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.embedders.register(name, f)
}

// RegisterClassifier registers a classifier implementation under name.
func (r *Registry) RegisterClassifier(name string, f Factory[model.Classifier]) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.classifiers.register(name, f)
}

// RegisterPolicy registers a trust policy under name.
func (r *Registry) RegisterPolicy(name string, f Factory[trust.Policy]) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.policies.register(name, f)
}

// RegisterHook registers a hook under name. Configuration attaches it to hook
// points.
func (r *Registry) RegisterHook(name string, f Factory[hook.Hook]) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hooks.register(name, f)
}

// RegisterSecretSource registers a secret source under name.
func (r *Registry) RegisterSecretSource(name string, f Factory[secret.Source]) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.secrets.register(name, f)
}

// RegisterSink registers a recording sink under name.
func (r *Registry) RegisterSink(name string, f Factory[record.Sink]) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sinks.register(name, f)
}

// RegisterMemory adds a memory backend under name.
func (r *Registry) RegisterMemory(name string, f Factory[memory.Backend]) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.memories.register(name, f)
}

// RegisterEvaluator registers an evaluator implementation under name. Package
// eval registers the evaluators bonyan ships with eval.Register.
func (r *Registry) RegisterEvaluator(name string, f Factory[score.Evaluator]) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.evaluators.register(name, f)
}

// RegisterQueue registers an evaluation queue implementation under name.
func (r *Registry) RegisterQueue(name string, f Factory[record.Queue]) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.queues.register(name, f)
}

func memQueue(options json.RawMessage) (record.Queue, error) {
	var o struct {
		Retention string `json:"retention"`
	}
	if len(options) > 0 {
		if err := json.Unmarshal(options, &o); err != nil {
			return nil, err
		}
	}
	retention, err := time.ParseDuration(o.Retention)
	if err != nil || retention <= 0 {
		return nil, fmt.Errorf(`options need a positive "retention", got %q`, o.Retention)
	}
	return record.NewMemQueue(retention)
}

// SlotConfig selects one implementation for a slot and carries its options.
type SlotConfig struct {
	Impl    string          `json:"impl"`
	Options json.RawMessage `json:"options,omitempty"`
}

// Config is the configuration an agent is assembled from. It is plain data a
// program loads however it likes; bonyan reads no file itself. The chat model is
// required; the other slots are optional. With no trust policy configured, the
// default policy is used.
type Config struct {
	Chat       SlotConfig  `json:"chat"`
	Embedder   *SlotConfig `json:"embedder,omitempty"`
	Classifier *SlotConfig `json:"classifier,omitempty"`
	Trust      *SlotConfig `json:"trust,omitempty"`
	// Hooks attaches hooks to points. At each point they run in the order
	// listed.
	Hooks map[hook.Point][]SlotConfig `json:"hooks,omitempty"`
	// Secrets lists the secret sources, tried in order. With none listed, the
	// environment is the only source.
	Secrets []SlotConfig `json:"secrets,omitempty"`
	// Recording selects the sink model calls and wrapper events are recorded
	// to. With none, nothing is recorded.
	Recording *SlotConfig `json:"recording,omitempty"`
	// Memory selects the memory backend. With none, the agent has no memory.
	Memory *MemoryConfig `json:"memory,omitempty"`
	// Evaluators lists the agent's evaluators (ADR 0001 §8). Each must have a
	// name of its own.
	Evaluators []SlotConfig `json:"evaluators,omitempty"`
	// Evaluation turns on live evaluation: finished runs are handed to a
	// queue, which an eval.Worker drains. With none, no run is queued.
	Evaluation *EvaluationConfig `json:"evaluation,omitempty"`
}

// EvaluationConfig selects the evaluation queue and how runs are handed to it
// (record.QueueOptions). A queued run is recorded without memory unless Full
// is set, whatever the recording keeps.
type EvaluationConfig struct {
	Queue       SlotConfig `json:"queue"`
	Rate        float64    `json:"rate"`
	MaxRunBytes int        `json:"max_run_bytes,omitempty"`
	Full        bool       `json:"full,omitempty"`
}

// MemoryConfig selects the memory backend and how long it keeps records.
type MemoryConfig struct {
	SlotConfig
	// Retention is how long a record is kept, as a Go duration such as
	// "720h". It is required.
	Retention string `json:"retention"`
}

// Components are the assembled parts. Every value is bonyan's wrapper around the
// configured implementation, never the implementation itself. An optional model
// slot that was not configured is nil. Trust, Hooks and Secrets are always set:
// with no policy configured Trust wraps the default policy, with no hooks Hooks
// runs none, and with no secret sources Secrets reads the environment.
type Components struct {
	Chat       model.Chat
	Embedder   model.Embedder
	Classifier model.Classifier
	Trust      trust.Policy
	Hooks      *Hooks
	// Secrets is the resolver over the configured sources. It resolves nothing
	// itself; a tool receives a scope of it.
	Secrets *secret.Resolver
	// Recorder records to the configured sink, scrubbing with Secrets'
	// scrubber. It is nil when no recording is configured. Chat and
	// classifier calls and the wrappers' events are recorded; embedder calls
	// are not.
	Recorder *record.Recorder
	// Memory is the store over the configured backend, classifying through
	// Trust. It is nil when no memory is configured.
	Memory *memory.Store
	// Evaluators are the configured evaluators, in order.
	Evaluators []score.Evaluator
	// EvalQueue is the evaluation queue the Recorder hands finished runs to.
	// It is nil when live evaluation is not configured.
	EvalQueue record.Queue

	// owned is a sink the registry opened, which Close closes.
	owned record.Sink
	// backend is the memory backend the registry built, which Close closes
	// when it can be closed.
	backend memory.Backend
}

// Close closes the recording sink if the registry opened it from
// configuration, and the memory backend if it can be closed. A sink passed
// with WithSink is the program's to close.
func (c Components) Close() error {
	var errs []error
	if c.owned != nil {
		errs = append(errs, c.owned.Close())
	}
	if cl, ok := c.backend.(io.Closer); ok {
		errs = append(errs, cl.Close())
	}
	return errors.Join(errs...)
}

// Hooks holds the hooks configuration attached to each point, in order.
type Hooks struct {
	byPoint map[hook.Point][]guardedHook
	scrub   *secret.Scrubber
	rec     events
}

// Only returns the hooks named, at the points they are attached to, and
// none of the others: what a re-run gets, since a hook runs in a re-run only
// when opted in by name (ADR 0001 §12). A nil Hooks gives nil.
func (h *Hooks) Only(names ...string) *Hooks {
	if h == nil {
		return nil
	}
	keep := map[string]bool{}
	for _, n := range names {
		keep[n] = true
	}
	out := &Hooks{byPoint: map[hook.Point][]guardedHook{}, scrub: h.scrub, rec: h.rec}
	for p, list := range h.byPoint {
		for _, g := range list {
			if keep[g.name] {
				out.byPoint[p] = append(out.byPoint[p], g)
			}
		}
	}
	return out
}

// Run calls the hooks attached to ev.Point in their configured order. ev is
// scrubbed before the first hook sees it; each hook sees the payload as the one
// before it left it, and the first denial ends the point (ADR 0001 §12). A nil
// Hooks runs none.
func (h *Hooks) Run(ctx context.Context, ev hook.Event) hook.Verdict {
	if h == nil || len(h.byPoint[ev.Point]) == 0 {
		return hook.Verdict{Event: ev}
	}
	v := hook.Verdict{Event: scrubEvent(ev, h.scrub)}
	for _, g := range h.byPoint[ev.Point] {
		next, changed, denied := g.run(ctx, v.Event)
		if denied {
			v.Denied = g.name
			return v
		}
		if changed {
			v.Event = next
			v.Changed = append(v.Changed, g.name)
		}
	}
	return v
}

// ApprovalVerdict is what the hooks at the approval point decided.
type ApprovalVerdict struct {
	Answer hook.Answer
	// By names the hook behind the answer: the one that approved, rejected
	// or said pending. It is empty when no hook decided.
	By string
}

// Approve runs the hooks at the approval point (ADR 0001 §12). Each approver
// answers in order, and the first rejection, or failure, ends the point. When
// none rejected and one said pending, the action is pending, whatever the
// others answered: a pending approver may still reject, so an approval alone
// does not let the action through, and the decision made later through the
// approval store answers for every approver that said pending. Otherwise the
// action is approved when an approver approved it, and undecided, answered
// Abstain, which cancels it. An observing hook only observes. Every outcome
// but a pending one is recorded here; a pending action's outcome is recorded
// with RecordApproval when it is decided.
func (h *Hooks) Approve(ctx context.Context, ev hook.Event) ApprovalVerdict {
	ev.Point = hook.Approval
	var approved, pending string
	if h != nil {
		ev = scrubEvent(ev, h.scrub)
		for _, g := range h.byPoint[hook.Approval] {
			switch g.answer(ctx, ev) {
			case hook.Reject:
				return ApprovalVerdict{Answer: hook.Reject, By: g.name}
			case hook.Approve:
				if approved == "" {
					approved = g.name
				}
			case hook.Pending:
				if pending == "" {
					pending = g.name
				}
			}
		}
	}
	switch {
	case pending != "":
		return ApprovalVerdict{Answer: hook.Pending, By: pending}
	case approved != "":
		h.RecordApproval(ctx, approved, DecisionApproved)
		return ApprovalVerdict{Answer: hook.Approve, By: approved}
	}
	h.RecordApproval(ctx, "", DecisionCancelled)
	return ApprovalVerdict{Answer: hook.Abstain}
}

// RecordApproval records an outcome at the approval point, with the hook that
// decided it, or none.
func (h *Hooks) RecordApproval(ctx context.Context, name, decision string) {
	if h == nil || h.rec == nil {
		return
	}
	h.rec.Event(ctx, record.Event{Slot: SlotHook, Name: name, Point: string(hook.Approval), Decision: decision})
}

// scrubEvent returns ev with every resolved secret removed from its content.
func scrubEvent(ev hook.Event, s *secret.Scrubber) hook.Event {
	ev.Message = content.From(ev.Message.Provenance(), s.Scrub(ev.Message.Raw()))
	if ev.Memory != nil {
		mem := make([]content.Text, len(ev.Memory))
		for i, t := range ev.Memory {
			mem[i] = s.ScrubText(t)
		}
		ev.Memory = mem
	}
	if ev.Messages != nil {
		msgs := make([]model.Message, len(ev.Messages))
		for i, m := range ev.Messages {
			parts := make([]content.Text, len(m.Parts))
			for j, p := range m.Parts {
				parts[j] = s.ScrubText(p)
			}
			m.Parts = parts
			m.ToolCalls = scrubCalls(m.ToolCalls, s)
			msgs[i] = m
		}
		ev.Messages = msgs
	}
	ev.Response.Content = s.Scrub(ev.Response.Content)
	ev.Response.ToolCalls = scrubCalls(ev.Response.ToolCalls, s)
	ev.Call.Arguments = scrubArgs(ev.Call.Arguments, s)
	ev.Result = content.From(ev.Result.Provenance(), s.Scrub(ev.Result.Raw()))
	ev.Reply = s.Scrub(ev.Reply)
	return ev
}

func scrubCalls(calls []model.ToolCall, s *secret.Scrubber) []model.ToolCall {
	if calls == nil {
		return nil
	}
	out := make([]model.ToolCall, len(calls))
	for i, c := range calls {
		c.Arguments = scrubArgs(c.Arguments, s)
		out[i] = c
	}
	return out
}

func scrubArgs(args json.RawMessage, s *secret.Scrubber) json.RawMessage {
	if args == nil {
		return nil
	}
	return json.RawMessage(s.Scrub(string(args)))
}

// Assemble builds every configured part and wraps it. Every implementation is
// built first; the recording is opened last, so a configuration that fails to
// assemble leaves no file behind.
func (r *Registry) Assemble(cfg Config, opts ...Option) (Components, error) {
	var a assembly
	for _, o := range opts {
		o(&a)
	}
	if a.sink != nil && cfg.Recording != nil {
		return Components{}, errTwoSinks
	}
	if a.sink == nil && cfg.Recording == nil {
		if cfg.Trust != nil && cfg.Trust.Impl != trust.DefaultName {
			return Components{}, fmt.Errorf("%w: trust policy %q", ErrNoRecorder, cfg.Trust.Impl)
		}
		if len(cfg.Hooks) > 0 {
			return Components{}, fmt.Errorf("%w: hooks are configured", ErrNoRecorder)
		}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	chat, err := r.chat.build(cfg.Chat)
	if err != nil {
		return Components{}, err
	}
	var emb model.Embedder
	if cfg.Embedder != nil {
		if emb, err = r.embedders.build(*cfg.Embedder); err != nil {
			return Components{}, err
		}
	}
	var cls model.Classifier
	if cfg.Classifier != nil {
		if cls, err = r.classifiers.build(*cfg.Classifier); err != nil {
			return Components{}, err
		}
	}

	policyCfg := SlotConfig{Impl: trust.DefaultName}
	if cfg.Trust != nil {
		policyCfg = *cfg.Trust
	}
	policy, err := r.policies.build(policyCfg)
	if err != nil {
		return Components{}, err
	}

	type attached struct {
		name string
		h    hook.Hook
	}
	hooks := map[hook.Point][]attached{}
	for point, list := range cfg.Hooks {
		if !point.Valid() {
			return Components{}, fmt.Errorf("registry: unknown hook point %q", point)
		}
		for _, hc := range list {
			h, err := r.hooks.build(hc)
			if err != nil {
				return Components{}, err
			}
			if _, ok := h.(hook.Interceptor); ok && hook.Rights(point) == 0 {
				return Components{}, fmt.Errorf("%w: hook %q may change or deny, and %q allows neither", ErrHookPoint, hc.Impl, point)
			}
			if _, ok := h.(hook.Approver); ok && point != hook.Approval {
				return Components{}, fmt.Errorf("%w: hook %q answers approvals, and %q is not the approval point", ErrHookPoint, hc.Impl, point)
			}
			hooks[point] = append(hooks[point], attached{name: hc.Impl, h: h})
		}
	}

	var backend memory.Backend
	var retention time.Duration
	if cfg.Memory != nil {
		if retention, err = time.ParseDuration(cfg.Memory.Retention); err != nil || retention <= 0 {
			return Components{}, fmt.Errorf("registry: memory retention %q: must be a positive duration", cfg.Memory.Retention)
		}
		if backend, err = r.memories.build(cfg.Memory.SlotConfig); err != nil {
			return Components{}, err
		}
	}

	evaluators := make([]score.Evaluator, 0, len(cfg.Evaluators))
	named := map[string]bool{}
	for _, ec := range cfg.Evaluators {
		e, err := r.evaluators.build(ec)
		if err != nil {
			return Components{}, err
		}
		if e == nil {
			return Components{}, fmt.Errorf("registry: evaluator %q: nil", ec.Impl)
		}
		// Asked once, so the name checked is the name it keeps.
		name := e.Name()
		if name == "" || named[name] {
			return Components{}, fmt.Errorf("registry: evaluator %q: no name, or a name another evaluator has", ec.Impl)
		}
		named[name] = true
		evaluators = append(evaluators, guardedEvaluator{name: name, inner: e})
	}

	secretCfgs := cfg.Secrets
	if len(secretCfgs) == 0 {
		secretCfgs = []SlotConfig{{Impl: SecretEnv}}
	}
	sources := make([]secret.Source, 0, len(secretCfgs))
	for _, sc := range secretCfgs {
		s, err := r.secrets.build(sc)
		if err != nil {
			return Components{}, err
		}
		sources = append(sources, s)
	}

	out := Components{Secrets: secret.NewResolver(sources...), Evaluators: evaluators}
	sink := a.sink
	if cfg.Recording != nil {
		if sink, err = r.sinks.build(*cfg.Recording); err != nil {
			return Components{}, err
		}
		out.owned = sink
	}
	var recOpts []record.Option
	if cfg.Evaluation != nil {
		q, err := r.queues.build(cfg.Evaluation.Queue)
		if err != nil {
			if out.owned != nil {
				_ = out.owned.Close()
			}
			return Components{}, err
		}
		out.EvalQueue = guardedQueue{inner: q}
		recOpts = append(recOpts, record.WithQueue(out.EvalQueue, record.QueueOptions{
			Rate: cfg.Evaluation.Rate, MaxRunBytes: cfg.Evaluation.MaxRunBytes, Full: cfg.Evaluation.Full,
		}))
		if sink == nil {
			// Runs are handed to the queue through the recorder, which needs
			// a sink; with no recording configured it keeps nothing else.
			sink = discardSink{}
		}
	}
	var ev events = discard{}
	if sink != nil {
		rec, err := record.NewRecorder(sink, out.Secrets.Scrubber(), recOpts...)
		if err != nil {
			if out.owned != nil {
				_ = out.owned.Close()
			}
			return Components{}, err
		}
		out.Recorder = rec
		ev = rec
		chat = record.Chat(chat, cfg.Chat.Impl, rec)
		if cls != nil {
			cls = record.Classifier(cls, cfg.Classifier.Impl, rec)
		}
	}

	out.Chat = guardChat(chat)
	if emb != nil {
		out.Embedder = guardEmbedder(emb)
	}
	if cls != nil {
		out.Classifier = guardClassifier(cls)
	}
	out.Trust = guardPolicy(policyCfg.Impl, policy, ev)
	if backend != nil {
		store, err := memory.NewStore(backend, memory.Options{Policy: out.Trust, PolicyName: policyCfg.Impl, Retention: retention})
		if err != nil {
			_ = out.Close()
			return Components{}, err
		}
		out.Memory, out.backend = store, backend
		if cfg.Recording != nil && cfg.Recording.Impl == SinkFile {
			dir, err := fileSinkDir(cfg.Recording.Options)
			if err != nil {
				_ = out.Close()
				return Components{}, err
			}
			store.OnDeleteSubject("full recordings", func(_ context.Context, subject string) error {
				return record.DeleteSubject(dir, subject)
			})
		}
		if out.EvalQueue != nil {
			store.OnDeleteSubject("evaluation queue", out.Recorder.DeleteSubject)
		}
	}
	out.Hooks = &Hooks{byPoint: map[hook.Point][]guardedHook{}, scrub: out.Secrets.Scrubber(), rec: ev}
	for point, list := range hooks {
		for _, x := range list {
			out.Hooks.byPoint[point] = append(out.Hooks.byPoint[point], guardedHook{name: x.name, inner: x.h, rec: ev, scrub: out.Hooks.scrub})
		}
	}
	return out, nil
}

// Names reports the registered names of a slot, sorted. For diagnostics.
func (r *Registry) Names(slotName string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	switch slotName {
	case SlotChat:
		return r.chat.names()
	case SlotEmbedder:
		return r.embedders.names()
	case SlotClassifier:
		return r.classifiers.names()
	case SlotTrust:
		return r.policies.names()
	case SlotHook:
		return r.hooks.names()
	case SlotSecret:
		return r.secrets.names()
	case SlotRecording:
		return r.sinks.names()
	case SlotMemory:
		return r.memories.names()
	}
	return nil
}
