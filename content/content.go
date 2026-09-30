// Package content separates text the program vouches for from text it merely
// carries.
//
// Trusted text is written by the program or its operator: system instructions,
// fixed templates. Untrusted text is text from a source the trust policy
// classifies as untrusted, and it always records where it came from. Under the
// default policy that is everything else that reaches a model: mail, web pages,
// feed items, uploads, tool output and memory recalled from any of those (see
// package trust).
//
// The two are distinct types, and this package offers no way to turn Untrusted
// into Trusted. Code that needs the characters of untrusted text reads them
// with Raw, a name chosen to stand out in review.
//
// What the types cannot do: Go cannot stop code that holds a plain string from
// passing it to Instruction, including a string it obtained from Raw. The
// guarantee is therefore about bonyan's own paths and API, which never convert,
// not about what a program chooses to do with raw strings.
package content

// Text is a piece of content that is either Trusted or Untrusted. It is sealed:
// only this package's types implement it, so every Text a caller holds is one
// of the two.
type Text interface {
	// Trusted reports whether the text was vouched for by the program.
	Trusted() bool
	sealed()
}

// Trusted is text the program or its operator wrote.
type Trusted struct {
	s string
}

// Instruction returns program-authored text as Trusted.
func Instruction(s string) Trusted { return Trusted{s: s} }

// String returns the text.
func (t Trusted) String() string { return t.s }

// Trusted reports true.
func (Trusted) Trusted() bool { return true }

func (Trusted) sealed() {}

// Kind classifies where untrusted text came from.
type Kind string

// The kinds of untrusted source. For recalled memory, Provenance.Origin keeps
// the kind of the material the fact was extracted from, so recall can report
// both that it is memory and what it originally was.
const (
	KindFetched Kind = "fetched" // retrieved from outside: mail, web, feeds
	KindUser    Kind = "user"    // supplied by an end user or an upload
	KindTool    Kind = "tool"    // returned by a tool call
	KindMemory  Kind = "memory"  // recalled memory derived from untrusted material
)

// Provenance records where untrusted text came from.
type Provenance struct {
	Kind Kind
	// Origin is set when Kind is KindMemory: the kind of the untrusted material
	// the recalled fact was extracted from (fetched, user or tool). It is empty
	// for every other kind.
	Origin Kind
	// ID identifies the specific source (a message id, a URL, a tool call id).
	// It is for tracing and audit, never for trust decisions.
	ID string
}

// Untrusted is text the program carries but does not vouch for.
type Untrusted struct {
	s    string
	from Provenance
}

// From returns s as Untrusted, recording where it came from.
func From(from Provenance, s string) Untrusted { return Untrusted{s: s, from: from} }

// Raw returns the characters of the untrusted text. It is named so that every
// use stands out: the result is a plain string and no longer carries the
// Untrusted type.
func (u Untrusted) Raw() string { return u.s }

// Provenance returns where the text came from.
func (u Untrusted) Provenance() Provenance { return u.from }

// Trusted reports false.
func (Untrusted) Trusted() bool { return false }

func (Untrusted) sealed() {}
