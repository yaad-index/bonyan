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

// Recall returns the facts containing every word of query, ignoring case,
// newest first.
func (b *Backend) Recall(_ context.Context, subject, query string, limit int, since time.Time) ([]memory.Record, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	words := strings.Fields(strings.ToLower(query))
	var out []memory.Record
	for _, r := range slices.Backward(b.records) {
		if len(out) == limit {
			break
		}
		if r.Layer != memory.LongTerm || r.Subject != subject || r.At.Before(since) {
			continue
		}
		text := strings.ToLower(r.Text)
		if !all(words, func(w string) bool { return strings.Contains(text, w) }) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
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

func all(words []string, f func(string) bool) bool {
	for _, w := range words {
		if !f(w) {
			return false
		}
	}
	return true
}
