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
	"sort"
	"sync"

	"github.com/yaad-index/bonyan/hook"
	"github.com/yaad-index/bonyan/model"
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
)

// ErrDuplicate reports a name already registered in a slot.
var ErrDuplicate = errors.New("registry: name already registered")

// ErrUnknown reports configuration naming an implementation that is not
// registered.
var ErrUnknown = errors.New("registry: unknown implementation")

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
	return impl, nil
}

// Registry holds the named implementations of every slot.
type Registry struct {
	mu          sync.RWMutex
	chat        *slot[model.Chat]
	embedders   *slot[model.Embedder]
	classifiers *slot[model.Classifier]
	policies    *slot[trust.Policy]
	hooks       *slot[hook.Hook]
}

// New returns a registry holding only the default trust policy, under
// trust.DefaultName.
func New() *Registry {
	r := &Registry{
		chat:        newSlot[model.Chat](SlotChat),
		embedders:   newSlot[model.Embedder](SlotEmbedder),
		classifiers: newSlot[model.Classifier](SlotClassifier),
		policies:    newSlot[trust.Policy](SlotTrust),
		hooks:       newSlot[hook.Hook](SlotHook),
	}
	r.policies.factories[trust.DefaultName] = func(json.RawMessage) (trust.Policy, error) {
		return trust.Default{}, nil
	}
	return r
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
}

// Components are the assembled parts. Every value is bonyan's wrapper around the
// configured implementation, never the implementation itself. An optional model
// slot that was not configured is nil. Trust and Hooks are always set: with no
// policy configured Trust wraps the default policy, and with no hooks Hooks runs
// none.
type Components struct {
	Chat       model.Chat
	Embedder   model.Embedder
	Classifier model.Classifier
	Trust      trust.Policy
	Hooks      *Hooks
}

// Hooks holds the hooks configuration attached to each point, in order.
type Hooks struct {
	byPoint map[hook.Point][]hook.Hook
}

// Run calls the hooks attached to ev.Point, in their configured order.
func (h *Hooks) Run(ctx context.Context, ev hook.Event) {
	for _, x := range h.byPoint[ev.Point] {
		_ = x.Observe(ctx, ev)
	}
}

// Assemble builds every configured part and wraps it.
func (r *Registry) Assemble(cfg Config, opts ...Option) (Components, error) {
	a := assembly{rec: discard{}}
	for _, o := range opts {
		o(&a)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	var out Components
	chat, err := r.chat.build(cfg.Chat)
	if err != nil {
		return Components{}, err
	}
	out.Chat = guardChat(chat)

	if cfg.Embedder != nil {
		e, err := r.embedders.build(*cfg.Embedder)
		if err != nil {
			return Components{}, err
		}
		out.Embedder = guardEmbedder(e)
	}
	if cfg.Classifier != nil {
		c, err := r.classifiers.build(*cfg.Classifier)
		if err != nil {
			return Components{}, err
		}
		out.Classifier = guardClassifier(c)
	}

	policyCfg := SlotConfig{Impl: trust.DefaultName}
	if cfg.Trust != nil {
		policyCfg = *cfg.Trust
	}
	p, err := r.policies.build(policyCfg)
	if err != nil {
		return Components{}, err
	}
	out.Trust = guardPolicy(policyCfg.Impl, p, a.rec)

	out.Hooks = &Hooks{byPoint: map[hook.Point][]hook.Hook{}}
	for point, list := range cfg.Hooks {
		if !point.Valid() {
			return Components{}, fmt.Errorf("registry: unknown hook point %q", point)
		}
		for _, hc := range list {
			h, err := r.hooks.build(hc)
			if err != nil {
				return Components{}, err
			}
			out.Hooks.byPoint[point] = append(out.Hooks.byPoint[point], guardedHook{name: hc.Impl, inner: h, rec: a.rec})
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
	}
	return nil
}
