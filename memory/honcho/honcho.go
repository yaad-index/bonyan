// Package honcho is a bonyan memory backend over the memory service from
// Plastic Labs (github.com/plastic-labs/honcho), reached over its v3 HTTP API
// as a separate service and never linked (ADR 0001 §4).
//
// A Backend keeps all of its namespace's memory in one workspace (ADR 0003),
// which records the namespace in its metadata; a workspace recording another
// namespace is refused. Each user is a peer named by the hex of the subject,
// with sessions of its own whose names begin with that peer's name, and the
// program is one peer that is never observed and observes nobody. Every
// record bonyan writes is a message carrying bonyan's fields in its metadata
// and the record's time as its own: an event in its session, a fact in the
// user's reserved facts session, which the service's deriver does not read.
// What the deriver concludes about the user comes back as derived facts, from
// the user's messages and written by the service's model
// (memory.Record.Derived). Every call names the one workspace and none lists
// workspaces, so a token scoped to that workspace is enough.
//
// Recall reads only the user's own sessions and the conclusions the user's
// peer holds about itself, and checks every result again on read: a message
// from a session or peer not the user's, or a conclusion about another peer,
// is dropped.
//
// The service cannot delete a peer or a single message. Deleting a subject
// deletes the user's sessions, every conclusion the user's peer holds or is
// the subject of, and the user's peer card and the program's card about the
// user, and empties the peer's metadata; then it waits for the service's
// deriver to finish the user's work and deletes the conclusions again, so a
// fact derived while the erase ran does not remain. The peer itself remains,
// empty, named by the hex of the subject: that the subject once had memory in
// the namespace is the residue of an erase.
//
// The service deletes messages only a whole session at a time, so deleting
// what retention expired rewrites a session that straddles the cut: its newer
// messages are written to a new generation of the session first, and the older
// generations are deleted after. A purge interrupted between the two leaves
// both, read as one with nothing doubled, and the next purge finishes it; it
// never loses a newer record. Only writes made through the same Backend wait
// for a rewrite: a write from another process to a session being rewritten can
// be lost.
//
// A record's fields, the trust decision it was stored under included, are data
// the service holds: a message whose fields hold a value bonyan never writes
// is not read as a record, and is dropped by a rewrite, but whoever can write
// to the service can store a well-formed one. The Store still applies its
// policy to every record it reads, so a stored decision never makes a record
// more trusted than the policy allows; give the service no wider access than
// the memory itself.
package honcho

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/trust"
)

// agentPeer names the program's peer. A user's peer is named by hex, which
// never spells it, so no user's peer can be the program's.
const agentPeer = "agent"

// factsKey names the reserved sessions facts are kept in. It holds letters no
// hex encoding does, so no event session can be named the same.
const factsKey = "facts"

// DefaultPrefix starts the name of the workspace a Backend makes, unless it is
// given one.
const DefaultPrefix = "bonyan"

// nsKey is the workspace metadata key recording the namespace it holds.
const nsKey = "bonyan_namespace"

// policyName is the decision recorded on a derived fact: the service's, and
// untrusted.
const policyName = "honcho"

// Options configures a Backend.
type Options struct {
	// URL is the service's base URL, such as http://localhost:8000.
	URL string
	// Namespace is the memory's namespace (memory.Options.Namespace).
	Namespace string
	// Prefix starts the name of the workspace the Backend makes,
	// <prefix>--<hex of the namespace>; empty means DefaultPrefix. It cannot be
	// set with Workspace.
	Prefix string
	// Workspace names the workspace instead, such as one an operator made and
	// scoped a token to. A workspace with no namespace recorded is taken for
	// Namespace; one recording another namespace is refused.
	Workspace string
	// Derive turns the service's deriver on for the user's messages.
	Derive bool
	// Instructions steer the deriver, as the workspace's custom instructions.
	// They steer it and enforce nothing.
	Instructions string
	// Token authenticates to the service, when it requires it.
	Token string
	// HTTPClient makes the calls; nil is a client with a 30 second timeout.
	HTTPClient *http.Client
	// DeleteWait bounds how long deleting a subject waits for the service's
	// deriver to finish the subject's work; zero is 30 seconds.
	DeleteWait time.Duration
}

// Backend is a memory.Backend over the service.
type Backend struct {
	c          *client
	ns         string
	ws         string
	cfg        configuration
	deleteWait time.Duration

	mu      sync.Mutex
	locks   map[string]*sync.RWMutex
	wsReady bool
	peers   map[string]bool
}

// idPattern is what the service accepts as an ID.
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,512}$`)

// maxID is the longest ID the service accepts.
const maxID = 512

// Open returns a backend over the service at opts.URL.
func Open(opts Options) (*Backend, error) {
	if opts.URL == "" {
		return nil, errors.New("honcho: empty URL")
	}
	ws := opts.Workspace
	switch {
	case ws != "" && opts.Prefix != "":
		return nil, errors.New("honcho: a prefix and a workspace name: give one")
	case ws != "":
		if !idPattern.MatchString(ws) {
			return nil, fmt.Errorf("honcho: workspace %q: letters, digits, dashes and underscores only, at most %d", ws, maxID)
		}
	default:
		prefix := opts.Prefix
		if prefix == "" {
			prefix = DefaultPrefix
		}
		if !idPattern.MatchString(prefix) || strings.Contains(prefix, "--") {
			return nil, fmt.Errorf("honcho: prefix %q: letters, digits, single dashes and underscores only", prefix)
		}
		ws = prefix + "--" + hex.EncodeToString([]byte(opts.Namespace))
		if !idPattern.MatchString(ws) {
			return nil, fmt.Errorf("honcho: the prefix and namespace are too long for a workspace name (%d characters, at most %d)", len(ws), maxID)
		}
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	wait := opts.DeleteWait
	if wait <= 0 {
		wait = 30 * time.Second
	}
	return &Backend{
		c:          &client{base: strings.TrimRight(opts.URL, "/"), http: hc, token: opts.Token},
		ns:         opts.Namespace,
		ws:         ws,
		cfg:        configuration{Reasoning: reasoning{Enabled: opts.Derive, CustomInstructions: opts.Instructions}},
		deleteWait: wait,
		locks:      map[string]*sync.RWMutex{},
		peers:      map[string]bool{},
	}, nil
}

// factoryOptions are a Backend's options in a configuration file.
type factoryOptions struct {
	URL          string `json:"url"`
	Prefix       string `json:"prefix"`
	Workspace    string `json:"workspace"`
	Derive       bool   `json:"derive"`
	Instructions string `json:"instructions"`
}

// Factory builds a Backend from configuration options, opened with namespace,
// for registry.Registry.RegisterMemory. A token is not configured this way:
// a program needing one opens the Backend itself.
func Factory(namespace string, options json.RawMessage) (memory.Backend, error) {
	var o factoryOptions
	if len(options) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(options)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&o); err != nil {
			return nil, fmt.Errorf("honcho: options: %w", err)
		}
	}
	return Open(Options{URL: o.URL, Namespace: namespace, Prefix: o.Prefix, Workspace: o.Workspace, Derive: o.Derive, Instructions: o.Instructions})
}

// Namespace is the namespace b was opened with.
func (b *Backend) Namespace() string { return b.ns }

// longestGen is the widest generation suffix a session name is checked with,
// so a name that fits at its first generation still fits at a later one.
const longestGen = "-g999999999"

// peerOf names subject's peer, refusing a subject too long for a peer name.
// The error gives lengths, never the subject.
func peerOf(subject string) (string, error) {
	p := hex.EncodeToString([]byte(subject))
	if subject == "" || len(p) > maxID {
		return "", fmt.Errorf("honcho: the subject is too long for a peer name (%d characters, at most %d)", len(p), maxID)
	}
	return p, nil
}

// checkSession refuses a session whose name under peer, at any generation,
// would be too long for the service. The error gives lengths, never names.
func checkSession(peer, k string) error {
	if n := len(peer) + 2 + len(k) + len(longestGen); n > maxID {
		return fmt.Errorf("honcho: the subject and session are too long for a session name (%d characters, at most %d)", n, maxID)
	}
	return nil
}

// lock is the lock of a user's peer: writes hold it shared, a purge rewriting
// the user's sessions and an erase hold it alone.
func (b *Backend) lock(peer string) *sync.RWMutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.locks[peer]
	if !ok {
		l = &sync.RWMutex{}
		b.locks[peer] = l
	}
	return l
}

// errForeign is a workspace that records another namespace than the
// Backend's.
var errForeign = errors.New("honcho: the workspace holds another namespace")

// workspace makes the workspace when it is missing and checks the namespace it
// records, once per Backend: a workspace with none recorded is taken for this
// namespace, one recording another is refused.
func (b *Backend) workspace(ctx context.Context) error {
	b.mu.Lock()
	ready := b.wsReady
	b.mu.Unlock()
	if ready {
		return nil
	}
	w, err := b.c.createWorkspace(ctx, b.ws, b.ns, b.cfg)
	if err != nil {
		return err
	}
	switch ns, ok := w.Metadata[nsKey]; {
	case !ok:
		meta := maps.Clone(w.Metadata)
		if meta == nil {
			meta = map[string]any{}
		}
		meta[nsKey] = b.ns
		if err := b.c.updateWorkspace(ctx, b.ws, meta, b.cfg); err != nil {
			return err
		}
	case ns != b.ns:
		return errForeign
	}
	b.mu.Lock()
	b.wsReady = true
	b.mu.Unlock()
	return nil
}

// ensure makes the workspace, the program's peer and peer, once per Backend.
func (b *Backend) ensure(ctx context.Context, peer string) error {
	if err := b.workspace(ctx); err != nil {
		return err
	}
	for _, p := range []string{agentPeer, peer} {
		b.mu.Lock()
		done := b.peers[p]
		b.mu.Unlock()
		if done {
			continue
		}
		if err := b.c.createPeer(ctx, b.ws, p); err != nil {
			return err
		}
		b.mu.Lock()
		b.peers[p] = true
		b.mu.Unlock()
	}
	return nil
}

// forget drops what the Backend remembers making, so the next write makes it
// again: after another process deleted the workspace.
func (b *Backend) forget() {
	b.mu.Lock()
	b.wsReady = false
	clear(b.peers)
	b.mu.Unlock()
}

// fields are bonyan's fields of a record, kept in its message's metadata.
type fields struct {
	Layer   memory.Layer  `json:"layer"`
	Session string        `json:"session,omitempty"`
	Origin  content.Kind  `json:"origin"`
	Server  string        `json:"server,omitempty"`
	Verdict trust.Verdict `json:"verdict"`
	Policy  string        `json:"policy"`
	// Record is the record's ID once a rewrite has copied it: the ID of the
	// message it was first written as.
	Record string `json:"record,omitempty"`
}

const metaKey = "bonyan"

// valid reports whether f holds only values bonyan writes: a known layer,
// source and verdict (none read as trusted by the Store), a session for an event and none for a fact, and a
// server only for remote tool output.
func (f fields) valid() bool {
	switch f.Layer {
	case memory.ShortTerm:
		if f.Session == "" {
			return false
		}
	case memory.LongTerm:
		if f.Session != "" {
			return false
		}
	default:
		return false
	}
	switch f.Origin {
	case content.KindFetched, content.KindUser, content.KindTool, content.KindRemoteTool, content.KindModel:
	default:
		return false
	}
	if f.Server != "" && f.Origin != content.KindRemoteTool {
		return false
	}
	switch f.Verdict {
	case trust.NoDecision, trust.Untrusted, trust.Trusted:
		return true
	}
	return false
}

func (f fields) metadata() map[string]any {
	b, _ := json.Marshal(f)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return map[string]any{metaKey: m}
}

// recordOf reads a message bonyan wrote back as a record of subject. A message
// bonyan did not write is not one.
func recordOf(subject string, m message) (memory.Record, bool) {
	raw, ok := m.Metadata[metaKey]
	if !ok {
		return memory.Record{}, false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return memory.Record{}, false
	}
	var f fields
	if err := json.Unmarshal(b, &f); err != nil || !f.valid() {
		return memory.Record{}, false
	}
	id := f.Record
	if id == "" {
		id = m.ID
	}
	return memory.Record{
		ID: "m-" + id, Layer: f.Layer, Subject: subject, Session: f.Session, Origin: f.Origin, Server: f.Server,
		Text: m.Content, At: m.CreatedAt.UTC(), Decision: memory.Decision{Verdict: f.Verdict, Policy: f.Policy},
	}, true
}

// derived reads a conclusion as a derived fact about subject.
func derived(subject string, c conclusion) memory.Record {
	return memory.Record{
		ID: "c-" + c.ID, Layer: memory.LongTerm, Subject: subject, Origin: content.KindUser, Derived: true,
		Text: c.Content, At: c.CreatedAt.UTC(), Decision: memory.Decision{Verdict: trust.Untrusted, Policy: policyName},
	}
}

// key names the sessions a record belongs in: its session's, or the facts'.
func key(r memory.Record) string {
	if r.Layer == memory.LongTerm {
		return factsKey
	}
	return hex.EncodeToString([]byte(r.Session))
}

// sessionID names generation gen of peer's sessions under k. Neither a peer's
// name nor a key holds "--", so the name splits back unambiguously.
func sessionID(peer, k string, gen int) string { return peer + "--" + k + "-g" + strconv.Itoa(gen) }

// parseSession splits a session name into its peer, key and generation.
func parseSession(id string) (peer, k string, gen int, ok bool) {
	peer, rest, found := strings.Cut(id, "--")
	if !found || peer == "" {
		return "", "", 0, false
	}
	i := strings.LastIndex(rest, "-g")
	if i <= 0 {
		return "", "", 0, false
	}
	gen, err := strconv.Atoi(rest[i+2:])
	if err != nil || gen < 1 {
		return "", "", 0, false
	}
	return peer, rest[:i], gen, true
}

// generations lists the generations of peer's sessions, oldest first, by key.
// Only sessions named for peer count, whatever the service lists.
func (b *Backend) generations(ctx context.Context, peer string) (map[string][]int, error) {
	ss, err := b.c.peerSessions(ctx, b.ws, peer)
	if errors.Is(err, errNotFound) {
		return map[string][]int{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string][]int{}
	for _, s := range ss {
		if p, k, gen, ok := parseSession(s.ID); ok && p == peer {
			out[k] = append(out[k], gen)
		}
	}
	for k := range out {
		slices.Sort(out[k])
	}
	return out, nil
}

// Write stores r as a message, in the newest generation of its session. When
// the workspace is gone, as after another process deleted it, it is made again
// and the write tried once more.
func (b *Backend) Write(ctx context.Context, r memory.Record) (string, error) {
	peer, err := peerOf(r.Subject)
	if err != nil {
		return "", err
	}
	if err := checkSession(peer, key(r)); err != nil {
		return "", err
	}
	l := b.lock(peer)
	l.RLock()
	defer l.RUnlock()
	id, err := b.write(ctx, peer, r)
	if errors.Is(err, errNotFound) {
		b.forget()
		id, err = b.write(ctx, peer, r)
	}
	return id, err
}

func (b *Backend) write(ctx context.Context, peer string, r memory.Record) (string, error) {
	if err := b.ensure(ctx, peer); err != nil {
		return "", err
	}
	gens, err := b.generations(ctx, peer)
	if err != nil {
		return "", err
	}
	k := key(r)
	gen := 1
	if g := gens[k]; len(g) > 0 {
		gen = g[len(g)-1]
	}
	s := sessionID(peer, k, gen)
	if err := b.c.createSession(ctx, b.ws, s, peer); err != nil {
		return "", err
	}
	msg := newMessage{Content: r.Text, PeerID: peer, CreatedAt: r.At.UTC(), Metadata: fields{
		Layer: r.Layer, Session: r.Session, Origin: r.Origin, Server: r.Server, Verdict: r.Decision.Verdict, Policy: r.Decision.Policy,
	}.metadata()}
	if r.Origin == content.KindModel {
		msg.PeerID = agentPeer
	}
	if r.Layer == memory.LongTerm {
		// A fact a program remembers is not material for the deriver.
		msg.Configuration = &configuration{Reasoning: reasoning{Enabled: false}}
	}
	got, err := b.c.addMessages(ctx, b.ws, s, []newMessage{msg})
	if err != nil {
		return "", err
	}
	if len(got) != 1 {
		return "", fmt.Errorf("honcho: add messages: %d messages back for one", len(got))
	}
	return "m-" + got[0].ID, nil
}

// ownMessage reports whether m, read from one of peer's sessions, is one of
// that user's conversation: sent by the user's peer or the program's.
func ownMessage(peer string, m message) bool { return m.PeerID == peer || m.PeerID == agentPeer }

// read returns the records of every generation of peer's sessions under k,
// each once, oldest first.
func (b *Backend) read(ctx context.Context, peer, subject, k string, gens []int) ([]memory.Record, error) {
	seen := map[string]bool{}
	var out []memory.Record
	for _, g := range gens {
		msgs, err := b.c.listMessages(ctx, b.ws, sessionID(peer, k, g))
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			if !ownMessage(peer, m) {
				continue
			}
			r, ok := recordOf(subject, m)
			if !ok || seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			out = append(out, r)
		}
	}
	slices.SortStableFunc(out, func(a, b memory.Record) int { return a.At.Compare(b.At) })
	return out, nil
}

// History returns a session's events written at or after since, oldest first.
func (b *Backend) History(ctx context.Context, subject, session string, since time.Time) ([]memory.Record, error) {
	peer, err := peerOf(subject)
	if err != nil {
		return nil, err
	}
	if err := b.workspace(ctx); err != nil {
		return nil, err
	}
	gens, err := b.generations(ctx, peer)
	if err != nil {
		return nil, err
	}
	k := hex.EncodeToString([]byte(session))
	recs, err := b.read(ctx, peer, subject, k, gens[k])
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(recs, func(r memory.Record) bool { return r.Layer != memory.ShortTerm || r.At.Before(since) }), nil
}

// ownConclusion reports whether c is one the user's peer holds about itself:
// the only conclusions recalled for the user.
func ownConclusion(peer string, c conclusion) bool { return c.Observer == peer && c.Observed == peer }

// Recall returns at most limit facts written at or after since: the facts a
// program remembered and those the deriver formed about the user, matched by
// the service's semantic search, taking from each in turn so neither crowds
// the other out, and then remembered facts sharing a word with the query,
// which the search can miss until the service has embedded them; a fact whose
// text is the query comes first, and an empty query returns the newest.
func (b *Backend) Recall(ctx context.Context, subject, query string, limit int, since time.Time) ([]memory.Record, error) {
	peer, err := peerOf(subject)
	if err != nil {
		return nil, err
	}
	if err := b.workspace(ctx); err != nil {
		return nil, err
	}
	gens, err := b.generations(ctx, peer)
	if err != nil {
		return nil, err
	}
	facts, err := b.read(ctx, peer, subject, factsKey, gens[factsKey])
	if err != nil {
		return nil, err
	}
	keep := func(r memory.Record) bool { return r.Layer == memory.LongTerm && !r.At.Before(since) }
	var found []memory.Record
	if query == "" {
		found = append(found, facts...)
		cs, err := b.c.listConclusions(ctx, b.ws, selfFilter(peer))
		if err != nil && !errors.Is(err, errNotFound) {
			return nil, err
		}
		for _, c := range cs {
			if ownConclusion(peer, c) {
				found = append(found, derived(subject, c))
			}
		}
		found = slices.DeleteFunc(found, func(r memory.Record) bool { return !keep(r) })
		slices.SortStableFunc(found, func(a, b memory.Record) int { return b.At.Compare(a.At) })
		return found[:min(limit, len(found))], nil
	}
	var searched []memory.Record
	for _, g := range gens[factsKey] {
		msgs, err := b.c.searchMessages(ctx, b.ws, sessionID(peer, factsKey, g), query)
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			if !ownMessage(peer, m) {
				continue
			}
			if r, ok := recordOf(subject, m); ok && keep(r) {
				searched = append(searched, r)
			}
		}
	}
	cs, err := b.c.queryConclusions(ctx, b.ws, query, selfFilter(peer))
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, err
	}
	var concluded []memory.Record
	for _, c := range cs {
		if !ownConclusion(peer, c) {
			continue
		}
		if r := derived(subject, c); keep(r) {
			concluded = append(concluded, r)
		}
	}
	for i := 0; i < max(len(searched), len(concluded)); i++ {
		if i < len(searched) {
			found = append(found, searched[i])
		}
		if i < len(concluded) {
			found = append(found, concluded[i])
		}
	}
	// The service embeds a message after it is written, so its search can
	// miss a fact for a while: a fact sharing a word with the query is found
	// by it all the same.
	for _, r := range slices.Backward(facts) {
		if keep(r) && sharesWord(query, r.Text) {
			found = append(found, r)
		}
	}
	seen := map[string]bool{}
	found = slices.DeleteFunc(found, func(r memory.Record) bool {
		dup := seen[r.ID]
		seen[r.ID] = true
		return dup
	})
	slices.SortStableFunc(found, func(a, b memory.Record) int {
		switch {
		case a.Text == query && b.Text != query:
			return -1
		case b.Text == query && a.Text != query:
			return 1
		}
		return 0
	})
	return found[:min(limit, len(found))], nil
}

// sharesWord reports whether text holds a whole word of query, ignoring case.
func sharesWord(query, text string) bool {
	tw := wordsOf(text)
	for _, w := range wordsOf(query) {
		if slices.Contains(tw, w) {
			return true
		}
	}
	return false
}

// wordsOf are the words of s, lower-cased: its runs of letters and digits.
func wordsOf(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// DeleteSubject erases subject from the workspace (ADR 0003 §3): its sessions,
// with every record in them; every conclusion its peer holds or is the
// subject of; its peer card and the program's card about it; and its peer's
// metadata. It then waits for the service's deriver to finish the subject's
// work and deletes the conclusions again, so nothing derived while it ran
// remains. Waiting longer than DeleteWait fails, and deleting again finishes
// the erase. The peer itself remains, empty: the service cannot delete one.
func (b *Backend) DeleteSubject(ctx context.Context, subject string) error {
	peer, err := peerOf(subject)
	if err != nil {
		return err
	}
	if err := b.workspace(ctx); err != nil {
		return err
	}
	l := b.lock(peer)
	l.Lock()
	defer l.Unlock()
	if err := b.deleteSessions(ctx, peer); err != nil {
		return err
	}
	if err := b.deleteConclusions(ctx, peer); err != nil {
		return err
	}
	for _, c := range []struct{ observer, target string }{{peer, ""}, {agentPeer, peer}} {
		if err := b.c.setCard(ctx, b.ws, c.observer, c.target); err != nil && !errors.Is(err, errNotFound) {
			return err
		}
	}
	if err := b.c.updatePeer(ctx, b.ws, peer); err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	if err := b.drained(ctx, peer); err != nil {
		return err
	}
	return b.deleteConclusions(ctx, peer)
}

// deleteSessions deletes every session of peer.
func (b *Backend) deleteSessions(ctx context.Context, peer string) error {
	gens, err := b.generations(ctx, peer)
	if err != nil {
		return err
	}
	for k, gs := range gens {
		for _, g := range gs {
			if err := b.c.deleteSession(ctx, b.ws, sessionID(peer, k, g)); err != nil && !errors.Is(err, errNotFound) {
				return err
			}
		}
	}
	return nil
}

// deleteConclusions deletes every conclusion peer holds or is the subject of,
// whoever holds it.
func (b *Backend) deleteConclusions(ctx context.Context, peer string) error {
	for _, filter := range []map[string]any{{"observer_id": peer}, {"observed_id": peer}} {
		cs, err := b.c.listConclusions(ctx, b.ws, filter)
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		for _, c := range cs {
			if c.Observer != peer && c.Observed != peer {
				continue
			}
			if err := b.c.deleteConclusion(ctx, b.ws, c.ID); err != nil && !errors.Is(err, errNotFound) {
				return err
			}
		}
	}
	return nil
}

// drained waits until the service's queue holds no work sent by peer or
// observed by it, at most DeleteWait.
func (b *Backend) drained(ctx context.Context, peer string) error {
	ctx, cancel := context.WithTimeout(ctx, b.deleteWait)
	defer cancel()
	pause := 50 * time.Millisecond
	for {
		busy := false
		for _, q := range []url.Values{{"sender_id": {peer}}, {"observer_id": {peer}}} {
			n, err := b.c.queued(ctx, b.ws, q)
			if err != nil && !errors.Is(err, errNotFound) {
				if ctx.Err() != nil {
					break
				}
				return err
			}
			busy = busy || n > 0
		}
		if !busy && ctx.Err() == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("honcho: the service's deriver still had work for the subject after %s; deleting the subject again finishes the erase", b.deleteWait)
		case <-time.After(pause):
		}
		pause = min(2*pause, time.Second)
	}
}

// DeleteBefore deletes every record of the namespace written before t, and
// every derived fact formed before it, inside the namespace's workspace.
func (b *Backend) DeleteBefore(ctx context.Context, t time.Time) error {
	if err := b.workspace(ctx); err != nil {
		return err
	}
	cs, err := b.c.listConclusions(ctx, b.ws, nil)
	if err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	for _, c := range cs {
		if c.CreatedAt.Before(t) {
			if err := b.c.deleteConclusion(ctx, b.ws, c.ID); err != nil && !errors.Is(err, errNotFound) {
				return err
			}
		}
	}
	ss, err := b.c.listSessions(ctx, b.ws)
	if err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	type group struct{ peer, k string }
	gens := map[group][]int{}
	for _, s := range ss {
		if p, k, gen, ok := parseSession(s.ID); ok {
			gens[group{p, k}] = append(gens[group{p, k}], gen)
		}
	}
	var errs []error
	for g, gs := range gens {
		slices.Sort(gs)
		l := b.lock(g.peer)
		l.Lock()
		err := b.rewrite(ctx, g.peer, g.k, gs, t)
		l.Unlock()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// maxBatch is the most messages the service takes in one call.
const maxBatch = 100

// rewrite leaves peer's sessions under k holding only the records written at or
// after t. A message bonyan did not write is not a record, and is not kept. When every record is older, every generation is deleted. When some
// are, or an earlier rewrite was left half done, the newer records go to a new
// generation first, and the older generations are deleted only once it holds
// them all, so a rewrite cut short loses nothing.
func (b *Backend) rewrite(ctx context.Context, peer, k string, gens []int, t time.Time) error {
	var all []message
	for _, g := range gens {
		msgs, err := b.c.listMessages(ctx, b.ws, sessionID(peer, k, g))
		if err != nil && !errors.Is(err, errNotFound) {
			return err
		}
		all = append(all, msgs...)
	}
	seen := map[string]bool{}
	var keep []message
	old := false
	for _, m := range all {
		r, ok := recordOf("", m)
		if !ok || !ownMessage(peer, m) || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		if r.At.Before(t) {
			old = true
			continue
		}
		keep = append(keep, m)
	}
	if !old && len(gens) == 1 {
		return nil
	}
	if len(keep) > 0 {
		next := sessionID(peer, k, gens[len(gens)-1]+1)
		if err := b.c.createSession(ctx, b.ws, next, peer); err != nil {
			return err
		}
		slices.SortStableFunc(keep, func(a, b message) int { return a.CreatedAt.Compare(b.CreatedAt) })
		copies := make([]newMessage, 0, len(keep))
		for _, m := range keep {
			r, _ := recordOf("", m)
			meta := m.Metadata[metaKey].(map[string]any)
			meta["record"] = strings.TrimPrefix(r.ID, "m-")
			copies = append(copies, newMessage{
				Content: m.Content, PeerID: m.PeerID, CreatedAt: m.CreatedAt.UTC(), Metadata: m.Metadata,
				// Copied, not new: the deriver read it when it was written.
				Configuration: &configuration{Reasoning: reasoning{Enabled: false}},
			})
		}
		for chunk := range slices.Chunk(copies, maxBatch) {
			got, err := b.c.addMessages(ctx, b.ws, next, chunk)
			if err != nil {
				return err
			}
			if len(got) != len(chunk) {
				return fmt.Errorf("honcho: add messages: %d of %d copied", len(got), len(chunk))
			}
		}
	}
	for _, g := range gens {
		if err := b.c.deleteSession(ctx, b.ws, sessionID(peer, k, g)); err != nil && !errors.Is(err, errNotFound) {
			return err
		}
	}
	return nil
}
