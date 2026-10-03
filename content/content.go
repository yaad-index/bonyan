// Package content separates text the program vouches for from text it merely
// carries.
//
// Trusted text is written by the program or its operator: system instructions,
// fixed templates. Untrusted text is text from a source the trust policy
// classifies as untrusted, and it always records where it came from. Under the
// default policy that is everything else that reaches a model: mail, web pages,
// feed items, uploads, tool output, a model's output read back into a request
// and memory recalled from any of those (see package trust).
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

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Text is a piece of content that is either Trusted or Untrusted. It is sealed:
// only this package's types implement it, so every Text a caller holds is one
// of the two.
type Text interface {
	// Trusted reports whether the text was vouched for by the program.
	Trusted() bool
	sealed()
}

// Trusted is text the program or its operator wrote, or text from a source
// the trust policy declared trusted. The second keeps where it came from.
type Trusted struct {
	s    string
	from Provenance
}

// Instruction returns program-authored text as Trusted.
func Instruction(s string) Trusted { return Trusted{s: s} }

// TrustedFrom returns text the trust policy declared trusted, keeping its
// provenance, so that what came from memory can still be told apart from what
// the program wrote. Only the code applying a policy's decision should call
// it.
func TrustedFrom(from Provenance, s string) Trusted { return Trusted{s: s, from: from} }

// Provenance is where the text came from; it is zero for text the program
// wrote.
func (t Trusted) Provenance() Provenance { return t.from }

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
	KindFetched    Kind = "fetched"     // retrieved from outside: mail, web, feeds
	KindUser       Kind = "user"        // supplied by an end user or an upload
	KindTool       Kind = "tool"        // returned by a tool call in the program
	KindRemoteTool Kind = "remote-tool" // returned by a tool on a tool server
	KindModel      Kind = "model"       // a model's output read back into a request
	KindMemory     Kind = "memory"      // recalled memory derived from untrusted material
)

// Provenance records where untrusted text came from.
type Provenance struct {
	Kind Kind
	// Origin is set when Kind is KindMemory: the kind of the untrusted material
	// the recalled fact was extracted from (fetched, user, tool, remote tool
	// or model). It is empty for every other kind.
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

// Section is untrusted material grouped under a label: the only way untrusted
// text enters assembled context (ADR 0001 §3). It is untrusted as a whole.
type Section struct {
	label string
	items []Untrusted
}

// NewSection groups items under label.
func NewSection(label string, items ...Untrusted) Section {
	return Section{label: label, items: append([]Untrusted(nil), items...)}
}

// Label is what the section holds, such as "material" or "tool result".
func (s Section) Label() string { return s.label }

// Items returns the section's untrusted items, in order.
func (s Section) Items() []Untrusted { return append([]Untrusted(nil), s.items...) }

// Trusted is always false: a section holds untrusted material.
func (Section) Trusted() bool { return false }

func (Section) sealed() {}

// Render returns the section with the default marking under a nonce over its
// own text: a labelled opening and closing line around it and a source line
// before each item, every one carrying the nonce. An item can therefore
// neither close its section nor pass part of itself off as another item, and
// the same section always renders the same way. The enforcement point marks a
// whole request under one nonce instead, so no item can hold another
// section's lines either.
func (s Section) Render() string {
	open, closing, header := DefaultMarking(s.label, Nonce(s))
	// Compose cannot refuse these delimiters: each carries a nonce over the
	// section's own text, which no item of it can contain.
	text, _ := s.Compose(open, closing, header)
	return text
}

// Nonce returns the first 12 hex of a SHA-256 over every section's label and
// items. No item of those sections can contain a line carrying it.
func Nonce(sections ...Section) string {
	h := sha256.New()
	for _, s := range sections {
		_, _ = fmt.Fprintf(h, "\x01%s", s.label)
		for _, it := range s.items {
			_, _ = fmt.Fprintf(h, "\x00%s\x00%s\x00%s\x00%s", it.from.Kind, it.from.Origin, it.from.ID, it.s)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// DefaultMarking returns the default policy's delimiters for a section
// labelled label under nonce.
func DefaultMarking(label, nonce string) (open, closing string, header func(Provenance) string) {
	return fmt.Sprintf("<<untrusted %s %s>>", label, nonce),
		fmt.Sprintf("<<end untrusted %s %s>>", label, nonce),
		func(p Provenance) string {
			if p.ID == "" {
				return fmt.Sprintf("[source %s: %s]", nonce, p.Kind)
			}
			return fmt.Sprintf("[source %s: %s %s]", nonce, p.Kind, p.ID)
		}
}

// ErrUnsafeMarking reports delimiters that do not delimit: an empty one, or one
// that an item's own text contains.
var ErrUnsafeMarking = errors.New("content: marking does not delimit")

// Compose renders the section with the given delimiters: open, then each item
// after its header, then closing. It refuses delimiters that are empty or that
// occur in an item's text, since such an item could end its section early or
// pose as another item.
func (s Section) Compose(open, closing string, header func(Provenance) string) (string, error) {
	if open == "" || closing == "" || header == nil {
		return "", ErrUnsafeMarking
	}
	var b strings.Builder
	b.WriteString(open)
	b.WriteString("\n")
	for _, it := range s.items {
		hd := header(it.from)
		if hd == "" {
			return "", ErrUnsafeMarking
		}
		b.WriteString(hd)
		b.WriteString("\n")
		b.WriteString(it.s)
		b.WriteString("\n")
	}
	for _, it := range s.items {
		if strings.Contains(it.s, closing) || strings.Contains(it.s, open) {
			return "", ErrUnsafeMarking
		}
		for _, other := range s.items {
			if strings.Contains(it.s, header(other.from)) {
				return "", ErrUnsafeMarking
			}
		}
	}
	b.WriteString(closing)
	return b.String(), nil
}

// Marked is a section as the enforcement point marked it for a model: its
// items, and the text the model is sent. Only bonyan's enforcement point
// builds one (ADR 0001 §3); it is untrusted as a whole.
type Marked struct {
	section Section
	text    string
}

// NewMarked pairs a section with the text it was marked as. It is for the
// enforcement point.
func NewMarked(s Section, text string) Marked { return Marked{section: s, text: text} }

// Section is the section that was marked.
func (m Marked) Section() Section { return m.section }

// Text is what the model is sent.
func (m Marked) Text() string { return m.text }

// Trusted is always false.
func (Marked) Trusted() bool { return false }

func (Marked) sealed() {}
