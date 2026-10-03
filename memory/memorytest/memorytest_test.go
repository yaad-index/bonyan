package memorytest_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/memory/inmem"
	"github.com/yaad-index/bonyan/memory/memorytest"
)

// nearMatch recalls as a backend matching by meaning would: every fact of the
// subject comes back for any query, the one whose text is the query first.
type nearMatch struct{ *inmem.Backend }

func (n nearMatch) Recall(ctx context.Context, subject, query string, limit int, since time.Time) ([]memory.Record, error) {
	all, err := n.Backend.Recall(ctx, subject, "", 1<<20, since)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(all, func(a, b memory.Record) int {
		switch {
		case a.Text == query && b.Text != query:
			return -1
		case b.Text == query && a.Text != query:
			return 1
		}
		return 0
	})
	return all[:min(limit, len(all))], nil
}

// A backend that also returns near matches passes the suite without the
// lexical checks: the contract every backend must meet does not hold it to
// matching by words.
func TestABackendMatchingByMeaningPassesWithoutLexical(t *testing.T) {
	memorytest.Run(t, func(*testing.T) memorytest.Open {
		s := inmem.NewStorage()
		return func(namespace string) memory.Backend { return nearMatch{s.Open(namespace)} }
	})
}
