// Package memory is bonyan's memory: per-session history and long-term facts
// about a subject, behind one interface every backend implements (ADR 0001
// §4).
//
// A program holds a Store, never a Backend directly. The Store applies the
// rules that are part of the interface rather than left to each backend: every
// record carries its subject, its source and the trust decision it was stored
// under, and that decision is the Store's own, made by the configured trust
// policy when the record is written. On every read the policy is applied again
// and the stricter of the two decisions wins, so nothing comes back more
// trusted than the material it came from. Records older than the retention
// period are never returned, and Purge deletes them.
//
// Long-term facts may be extracted asynchronously: a fact remembered during one
// turn is not promised to be recallable on the next.
package memory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/trust"
)

// Layer is where a record is kept.
type Layer string

// The layers.
const (
	ShortTerm Layer = "short-term" // a session's history, as events
	LongTerm  Layer = "long-term"  // facts about a subject, across sessions
)

// Decision is the trust decision a record was stored under.
type Decision struct {
	Verdict trust.Verdict
	// Policy is the name of the policy that made it.
	Policy string
}

// Record is one stored item. Every record carries its subject, its source and
// the decision it was stored under.
type Record struct {
	// ID is assigned by the backend when the record is written.
	ID      string
	Layer   Layer
	Subject string
	// Session is the session an event belongs to; it is empty for a fact.
	Session string
	// Origin is the kind of material the text came from: for a fact, the
	// material it was extracted from.
	Origin   content.Kind
	Text     string
	At       time.Time
	Decision Decision
}

// Backend stores records. It holds no rules of its own: the Store decides what
// is written and how what is read is trusted. Every method must be safe for
// concurrent use.
type Backend interface {
	// Write stores r and returns the ID it assigned.
	Write(ctx context.Context, r Record) (string, error)
	// History returns the events of one session of subject written at or
	// after since, oldest first.
	History(ctx context.Context, subject, session string, since time.Time) ([]Record, error)
	// Recall returns at most limit facts about subject written at or after
	// since that match query, most relevant first. A fact matches when it
	// holds any word of the query; an empty query matches every fact. How
	// well the matches are ranked is the backend's own: memory/sqlite ranks
	// by bm25, which weighs rare words above common ones, while memory/inmem
	// counts matched words, a reference rather than a recommendation.
	Recall(ctx context.Context, subject, query string, limit int, since time.Time) ([]Record, error)
	// DeleteSubject deletes every record of subject, events and facts alike.
	DeleteSubject(ctx context.Context, subject string) error
	// DeleteBefore deletes every record written before t.
	DeleteBefore(ctx context.Context, t time.Time) error
}

// ErrInvalid reports a call the Store refuses before it reaches the backend.
var ErrInvalid = errors.New("memory: invalid")

// Options configures a Store.
type Options struct {
	// Policy classifies every record as it is written and again as it is read.
	// It should come from the registry (registry.GuardPolicy or Assemble), so
	// its decisions are recorded; the Store treats any error or missing
	// decision as untrusted either way. Nil is the default policy.
	Policy trust.Policy
	// PolicyName is recorded with each decision.
	PolicyName string
	// Retention is how long a record is kept. It must be positive.
	Retention time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Store is memory as a program uses it.
type Store struct {
	backend   Backend
	policy    trust.Policy
	name      string
	retention time.Duration
	now       func() time.Time

	mu       sync.Mutex
	deleters []deleter
}

type deleter struct {
	name string
	del  func(ctx context.Context, subject string) error
}

// OnDeleteSubject adds del, under name, to what DeleteSubject deletes, for
// what is kept about a subject outside memory, such as full recordings (ADR
// 0001 §4).
func (s *Store) OnDeleteSubject(name string, del func(ctx context.Context, subject string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleters = append(s.deleters, deleter{name: name, del: del})
}

// NewStore returns a Store over b.
func NewStore(b Backend, opts Options) (*Store, error) {
	if b == nil {
		return nil, fmt.Errorf("%w: no backend", ErrInvalid)
	}
	if opts.Retention <= 0 {
		return nil, fmt.Errorf("%w: retention must be positive, got %s", ErrInvalid, opts.Retention)
	}
	s := &Store{backend: b, policy: opts.Policy, name: opts.PolicyName, retention: opts.Retention, now: opts.Now}
	if s.policy == nil {
		s.policy, s.name = trust.Default{}, trust.DefaultName
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// Append adds an event to a session's history. origin is the kind of material
// text came from.
func (s *Store) Append(ctx context.Context, subject, session string, origin content.Kind, text string) error {
	if session == "" {
		return fmt.Errorf("%w: empty session", ErrInvalid)
	}
	return s.write(ctx, Record{Layer: ShortTerm, Subject: subject, Session: session, Origin: origin, Text: text})
}

// Remember stores a fact about subject. origin is the kind of material the
// fact was extracted from.
func (s *Store) Remember(ctx context.Context, subject string, origin content.Kind, text string) error {
	return s.write(ctx, Record{Layer: LongTerm, Subject: subject, Origin: origin, Text: text})
}

func (s *Store) write(ctx context.Context, r Record) error {
	if r.Subject == "" {
		return fmt.Errorf("%w: empty subject", ErrInvalid)
	}
	if r.Origin == "" {
		return fmt.Errorf("%w: empty origin", ErrInvalid)
	}
	r.At = s.now()
	r.Decision = Decision{Verdict: s.classify(ctx, r.Origin), Policy: s.name}
	_, err := s.backend.Write(ctx, r)
	return err
}

// History returns a session's events, oldest first, as text whose trust is the
// stricter of the decision each was stored under and the policy's decision
// now.
func (s *Store) History(ctx context.Context, subject, session string) ([]content.Text, error) {
	if subject == "" || session == "" {
		return nil, fmt.Errorf("%w: empty subject or session", ErrInvalid)
	}
	recs, err := s.backend.History(ctx, subject, session, s.cutoff())
	if err != nil {
		return nil, err
	}
	return s.read(ctx, recs, ShortTerm, subject, session), nil
}

// Recall returns at most limit facts about subject matching query, most
// relevant first, trusted as History's events are.
func (s *Store) Recall(ctx context.Context, subject, query string, limit int) ([]content.Text, error) {
	if subject == "" {
		return nil, fmt.Errorf("%w: empty subject", ErrInvalid)
	}
	if limit <= 0 {
		return nil, fmt.Errorf("%w: limit must be positive, got %d", ErrInvalid, limit)
	}
	recs, err := s.backend.Recall(ctx, subject, query, limit, s.cutoff())
	if err != nil {
		return nil, err
	}
	if len(recs) > limit {
		recs = recs[:limit]
	}
	return s.read(ctx, recs, LongTerm, subject, ""), nil
}

// DeleteSubject deletes every record of subject, then everything added with
// OnDeleteSubject. Each is tried even when another fails, and any failure is
// returned naming what failed: a deletion is never reported as done when part
// of it is not.
func (s *Store) DeleteSubject(ctx context.Context, subject string) error {
	if subject == "" {
		return fmt.Errorf("%w: empty subject", ErrInvalid)
	}
	var errs []error
	if err := s.backend.DeleteSubject(ctx, subject); err != nil {
		errs = append(errs, fmt.Errorf("memory: deleting the subject from the backend: %w", err))
	}
	s.mu.Lock()
	dels := append([]deleter(nil), s.deleters...)
	s.mu.Unlock()
	for _, d := range dels {
		if err := d.del(ctx, subject); err != nil {
			errs = append(errs, fmt.Errorf("memory: deleting the subject from %s: %w", d.name, err))
		}
	}
	return errors.Join(errs...)
}

// Purge deletes every record older than the retention period.
func (s *Store) Purge(ctx context.Context) error {
	return s.backend.DeleteBefore(ctx, s.cutoff())
}

func (s *Store) cutoff() time.Time { return s.now().Add(-s.retention) }

// read turns what a backend returned into text. A record from another layer,
// subject or session, or older than the retention period, is left out,
// whatever the backend did.
func (s *Store) read(ctx context.Context, recs []Record, layer Layer, subject, session string) []content.Text {
	cutoff := s.cutoff()
	out := make([]content.Text, 0, len(recs))
	for _, r := range recs {
		if r.Layer != layer || r.Subject != subject || r.Session != session || r.At.Before(cutoff) {
			continue
		}
		// The policy runs on every read, whatever the stored decision, so each
		// recall's decision is made and recorded.
		now := s.classify(ctx, r.Origin)
		if r.Decision.Verdict == trust.Trusted && now == trust.Trusted {
			out = append(out, content.TrustedFrom(content.Provenance{Kind: content.KindMemory, Origin: r.Origin, ID: r.ID}, r.Text))
			continue
		}
		out = append(out, content.From(content.Provenance{Kind: content.KindMemory, Origin: r.Origin, ID: r.ID}, r.Text))
	}
	return out
}

// classify is the policy's verdict on material from memory that came from
// origin: trusted only when the policy says so without failing.
func (s *Store) classify(ctx context.Context, origin content.Kind) trust.Verdict {
	d, err := s.policy.Classify(ctx, content.Provenance{Kind: content.KindMemory, Origin: origin})
	if err != nil || d.Verdict != trust.Trusted {
		return trust.Untrusted
	}
	return trust.Trusted
}
