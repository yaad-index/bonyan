// Package secret resolves secrets for tools and keeps resolved values out of
// everything bonyan emits (ADR 0001 §10).
//
// Secrets come from pluggable sources, read at every lookup so a rotated value
// is picked up without a restart. A Resolver cannot resolve anything itself: it
// hands out Scoped resolvers, each limited to the names its configuration
// grants, and a tool receives only its Scoped. Every value resolved through any
// of them is added to the Resolver's Scrubber, which removes it by exact match
// from tool output and log output.
//
// What this cannot do: code in the program's process can still read the
// environment or the files directly, so scoping covers what goes through
// bonyan, not the process. Scrubbing is by exact match, so an encoded or partial
// secret passes it.
package secret

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
)

// Redacted replaces a secret wherever bonyan would otherwise print it.
const Redacted = "[REDACTED]"

// ErrNotFound reports a name a source does not hold.
var ErrNotFound = errors.New("secret: not found")

// ErrNotGranted reports a name outside a scoped resolver's grant.
var ErrNotGranted = errors.New("secret: not granted")

// Value is a resolved secret. Every way of printing or encoding it yields
// Redacted; only Reveal returns the secret.
type Value struct {
	s string
}

// Reveal returns the secret. It is named so that every use stands out.
func (v Value) Reveal() string { return v.s }

// String returns Redacted.
func (Value) String() string { return Redacted }

// GoString returns Redacted.
func (Value) GoString() string { return Redacted }

// Format writes Redacted for every verb.
func (Value) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, Redacted) }

// MarshalJSON encodes Redacted.
func (Value) MarshalJSON() ([]byte, error) { return json.Marshal(Redacted) }

// MarshalText encodes Redacted.
func (Value) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// LogValue logs Redacted.
func (Value) LogValue() slog.Value { return slog.StringValue(Redacted) }
