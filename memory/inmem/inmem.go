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

// Backend is an in-memory memory.Backend.
type Backend struct {
	mu      sync.Mutex
	next    int
	records []memory.Record
}

// New returns an empty backend.
func New() *Backend { return &Backend{} }

// Write stores r.
func (b *Backend) Write(_ context.Context, r memory.Record) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	r.ID = strconv.Itoa(b.next)
	b.records = append(b.records, r)
	return r.ID, nil
}

// History returns a session's events in the order they were written.
func (b *Backend) History(_ context.Context, subject, session string, since time.Time) ([]memory.Record, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []memory.Record
	for _, r := range b.records {
		if r.Layer == memory.ShortTerm && r.Subject == subject && r.Session == session && !r.At.Before(since) {
			out = append(out, r)
		}
	}
	return out, nil
}

// Recall returns the facts holding any word of query, ignoring case, those
// holding the most of its words first and then the newest; an empty query
// returns the newest facts.
func (b *Backend) Recall(_ context.Context, subject, query string, limit int, since time.Time) ([]memory.Record, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	words := queryWords(query)
	type scored struct {
		r     memory.Record
		score int
	}
	var found []scored
	for _, r := range slices.Backward(b.records) {
		if r.Layer != memory.LongTerm || r.Subject != subject || r.At.Before(since) {
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
	b.mu.Lock()
	defer b.mu.Unlock()
	b.records = slices.DeleteFunc(b.records, func(r memory.Record) bool { return r.Subject == subject })
	return nil
}

// DeleteBefore deletes every record written before t.
func (b *Backend) DeleteBefore(_ context.Context, t time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.records = slices.DeleteFunc(b.records, func(r memory.Record) bool { return r.At.Before(t) })
	return nil
}
