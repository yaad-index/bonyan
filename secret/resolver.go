package secret

import (
	"context"
	"errors"
	"fmt"
)

// Resolver holds the configured sources and the scrubber for every value
// resolved through it. It resolves nothing itself; Scope hands out resolvers
// limited to a grant.
type Resolver struct {
	sources []Source
	scrub   *Scrubber
}

// NewResolver returns a resolver that looks names up in sources, in order: the
// first source that holds a name answers.
func NewResolver(sources ...Source) *Resolver {
	return &Resolver{sources: sources, scrub: NewScrubber()}
}

// Scrubber returns the scrubber holding every value resolved through any of
// this resolver's scopes.
func (r *Resolver) Scrubber() *Scrubber { return r.scrub }

// Scope returns a resolver that can reach only the names in grant.
func (r *Resolver) Scope(grant ...string) *Scoped {
	g := make(map[string]struct{}, len(grant))
	for _, n := range grant {
		g[n] = struct{}{}
	}
	return &Scoped{r: r, grant: g}
}

// Scoped resolves the names it was granted and no others. It is what a tool
// receives, and it offers no way to widen its grant.
type Scoped struct {
	r     *Resolver
	grant map[string]struct{}
}

// Resolve looks name up and adds the value to the scrubber. A name outside the
// grant fails with ErrNotGranted before any source is read. A source failing
// with anything other than ErrNotFound ends the lookup with its error, and a
// source that panics fails it too.
func (s *Scoped) Resolve(ctx context.Context, name string) (v Value, err error) {
	if _, ok := s.grant[name]; !ok {
		return Value{}, fmt.Errorf("%w: %q", ErrNotGranted, name)
	}
	defer func() {
		if p := recover(); p != nil {
			v, err = Value{}, fmt.Errorf("secret: resolve %q: source panicked", name)
		}
	}()
	for _, src := range s.r.sources {
		val, err := src.Lookup(ctx, name)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return Value{}, fmt.Errorf("secret: resolve %q: %w", name, err)
		}
		s.r.scrub.add(val)
		return Value{s: val}, nil
	}
	return Value{}, fmt.Errorf("%w: %q", ErrNotFound, name)
}
