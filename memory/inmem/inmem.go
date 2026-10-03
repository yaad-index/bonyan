// Package inmem is a memory backend held in process memory: the reference
// backend the conformance suite runs against, and a store for tests and short
// runs. Nothing survives the process.
package inmem

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/yaad-index/bonyan/memory"
)

// Storage holds the records of every namespace opened on it.
type Storage struct {
	mu      sync.Mutex
	next    int
	records []stored
}

type stored struct {
	namespace string
	r         memory.Record
}

// NewStorage returns an empty storage.
func NewStorage() *Storage { return &Storage{} }

// Open returns a backend over s inside namespace.
func (s *Storage) Open(namespace string) *Backend { return &Backend{s: s, ns: namespace} }

// Backend is an in-memory memory.Backend inside one namespace of a Storage.
type Backend struct {
	s  *Storage
	ns string
}

// New returns a backend over an empty storage, opened with namespace.
func New(namespace string) *Backend { return NewStorage().Open(namespace) }

// Namespace is the namespace b was opened with.
func (b *Backend) Namespace() string { return b.ns }

// each calls fn with every record of b's namespace, under the storage's lock.
func (b *Backend) each(fn func(r memory.Record)) {
	for _, st := range b.s.records {
		if st.namespace == b.ns {
			fn(st.r)
		}
	}
}

// Write stores r.
func (b *Backend) Write(_ context.Context, r memory.Record) (string, error) {
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	b.s.next++
	r.ID = strconv.Itoa(b.s.next)
	b.s.records = append(b.s.records, stored{namespace: b.ns, r: r})
	return r.ID, nil
}

// History returns a session's events in the order they were written.
func (b *Backend) History(_ context.Context, subject, session string, since time.Time) ([]memory.Record, error) {
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	var out []memory.Record
	b.each(func(r memory.Record) {
		if r.Layer == memory.ShortTerm && r.Subject == subject && r.Session == session && !r.At.Before(since) {
			out = append(out, r)
		}
	})
	return out, nil
}

// Recall returns the facts holding any word of query, ignoring case, those
// holding the most of its words first and then the newest; an empty query
// returns the newest facts.
func (b *Backend) Recall(_ context.Context, subject, query string, limit int, since time.Time) ([]memory.Record, error) {
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	words := queryWords(query)
	type scored struct {
		r     memory.Record
		score int
	}
	var found []scored
	for _, st := range slices.Backward(b.s.records) {
		r := st.r
		if st.namespace != b.ns || r.Layer != memory.LongTerm || r.Subject != subject || r.At.Before(since) {
			continue
		}
		text := strings.ToLower(r.Text)
		score := 0
		for _, w := range words {
			if strings.Contains(text, w) {
				score++
			}
		}
		if len(words) > 0 && score == 0 {
			continue
		}
		found = append(found, scored{r, score})
	}
	slices.SortStableFunc(found, func(a, b scored) int { return b.score - a.score })
	out := make([]memory.Record, 0, min(limit, len(found)))
	for _, f := range found[:min(limit, len(found))] {
		out = append(out, f.r)
	}
	return out, nil
}

// queryWords are the distinct words of query, lower-cased, with what is not a
// letter or digit trimmed from each end.
func queryWords(query string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range strings.Fields(strings.ToLower(query)) {
		w = strings.TrimFunc(w, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		if w != "" && !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// DeleteSubject deletes every record of subject.
func (b *Backend) DeleteSubject(_ context.Context, subject string) error {
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	b.s.records = slices.DeleteFunc(b.s.records, func(st stored) bool { return st.namespace == b.ns && st.r.Subject == subject })
	return nil
}

// DeleteBefore deletes every record of the namespace written before t.
func (b *Backend) DeleteBefore(_ context.Context, t time.Time) error {
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	b.s.records = slices.DeleteFunc(b.s.records, func(st stored) bool { return st.namespace == b.ns && st.r.At.Before(t) })
	return nil
}
