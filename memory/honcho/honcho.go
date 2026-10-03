// Package honcho is a bonyan memory backend over the memory service from
// Plastic Labs (github.com/plastic-labs/honcho), reached over its v3 HTTP API
// as a separate service and never linked (ADR 0001 §4).
//
// Each subject is a workspace of its own, named by the namespace and the
// subject, holding a peer for the user and one for the program, so deleting
// the subject deletes the workspace: the service's only complete erase, since
// it cannot delete a peer or a single message. Every record bonyan writes is a
// message carrying bonyan's fields in its metadata and the record's time as its
// own: an event in its session, a fact in a reserved facts session the
// service's deriver does not read. What the deriver concludes about the user
// comes back as derived facts, from the user's messages and written by the
// service's model (memory.Record.Derived).
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
	"net/http"
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

// The peers of every workspace.
const (
	peerUser  = "user"
	peerAgent = "agent"
)

// factsKey names the reserved session facts are kept in. It holds letters no
// hex encoding does, so no event session can be named the same.
const factsKey = "facts"

// DefaultPrefix starts the name of every workspace the backend makes.
const DefaultPrefix = "bonyan"

// policyName is the decision recorded on a derived fact: the service's, and
// untrusted.
const policyName = "honcho"

// Options configures a Backend.
type Options struct {
	// URL is the service's base URL, such as http://localhost:8000.
	URL string
	// Namespace is the memory's namespace (memory.Options.Namespace).
	Namespace string
	// Prefix starts every workspace name, so the backend's workspaces can be
	// told from others on the same service; empty means DefaultPrefix.
	Prefix string
	// Derive turns the service's deriver on for the user's messages.
	Derive bool
	// Instructions steer the deriver, as the workspace's custom instructions.
	// They steer it and enforce nothing.
	Instructions string
	// Token authenticates to the service, when it requires it.
	Token string
	// HTTPClient makes the calls; nil is a client with a 30 second timeout.
	HTTPClient *http.Client
	// DeleteWait bounds how long deleting a subject waits for the service to
	// let its workspace go; zero is 30 seconds.
	DeleteWait time.Duration
}

// Backend is a memory.Backend over the service.
type Backend struct {
	c          *client
	ns         string
	prefix     string
	cfg        configuration
	deleteWait time.Duration

	mu    sync.Mutex
	locks map[string]*sync.RWMutex
	ready map[string]bool
}

// idPattern is what the service accepts as an ID.
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,512}$`)

// Open returns a backend over the service at opts.URL.
func Open(opts Options) (*Backend, error) {
	if opts.URL == "" {
		return nil, errors.New("honcho: empty URL")
	}
	prefix := opts.Prefix
	if prefix == "" {
		prefix = DefaultPrefix
	}
	if !idPattern.MatchString(prefix) || strings.Contains(prefix, "--") {
		return nil, fmt.Errorf("honcho: prefix %q: letters, digits, single dashes and underscores only", prefix)
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
		prefix:     prefix,
		cfg:        configuration{Reasoning: reasoning{Enabled: opts.Derive, CustomInstructions: opts.Instructions}},
		deleteWait: wait,
		locks:      map[string]*sync.RWMutex{},
		ready:      map[string]bool{},
	}, nil
}

// factoryOptions are a Backend's options in a configuration file.
type factoryOptions struct {
	URL          string `json:"url"`
	Prefix       string `json:"prefix"`
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
	return Open(Options{URL: o.URL, Namespace: namespace, Prefix: o.Prefix, Derive: o.Derive, Instructions: o.Instructions})
}

// Namespace is the namespace b was opened with.
func (b *Backend) Namespace() string { return b.ns }

// scope starts the name of every workspace of b's namespace.
func (b *Backend) scope() string { return b.prefix + "--" + hex.EncodeToString([]byte(b.ns)) + "--" }

// workspace names subject's workspace.
func (b *Backend) workspace(subject string) (string, error) {
	id := b.scope() + hex.EncodeToString([]byte(subject))
	if subject == "" || !idPattern.MatchString(id) {
		return "", fmt.Errorf("honcho: the namespace and subject are too long for a workspace name (%d characters)", len(id))
	}
	return id, nil
}

// lock is the lock of workspace ws: writes hold it shared, a purge rewriting
// its sessions holds it alone.
func (b *Backend) lock(ws string) *sync.RWMutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.locks[ws]
	if !ok {
		l = &sync.RWMutex{}
		b.locks[ws] = l
	}
	return l
}

// ensure makes ws and its peers, once per Backend.
func (b *Backend) ensure(ctx context.Context, ws string) error {
	b.mu.Lock()
	done := b.ready[ws]
	b.mu.Unlock()
	if done {
		return nil
	}
	if err := b.c.createWorkspace(ctx, ws, b.ns, b.cfg); err != nil {
		return err
	}
	for _, p := range []string{peerUser, peerAgent} {
		if err := b.c.createPeer(ctx, ws, p); err != nil {
			return err
		}
	}
	b.mu.Lock()
	b.ready[ws] = true
	b.mu.Unlock()
	return nil
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

// sessionID names generation gen of the sessions under k.
func sessionID(k string, gen int) string { return k + "-g" + strconv.Itoa(gen) }

// parseSession splits a session name into its key and generation.
func parseSession(id string) (string, int, bool) {
	i := strings.LastIndex(id, "-g")
	if i <= 0 {
		return "", 0, false
	}
	gen, err := strconv.Atoi(id[i+2:])
	if err != nil || gen < 1 {
		return "", 0, false
	}
	return id[:i], gen, true
}

// generations lists the generations of every session of ws, oldest first, by
// key.
func (b *Backend) generations(ctx context.Context, ws string) (map[string][]int, error) {
	ss, err := b.c.listSessions(ctx, ws)
	if errors.Is(err, errNotFound) {
		return map[string][]int{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string][]int{}
	for _, s := range ss {
		if k, gen, ok := parseSession(s.ID); ok {
			out[k] = append(out[k], gen)
		}
	}
	for k := range out {
		slices.Sort(out[k])
	}
	return out, nil
}

// Write stores r as a message, in the newest generation of its session. When
// the workspace is gone, as after another process deleted the subject, it is
// made again and the write tried once more.
func (b *Backend) Write(ctx context.Context, r memory.Record) (string, error) {
	ws, err := b.workspace(r.Subject)
	if err != nil {
		return "", err
	}
	l := b.lock(ws)
	l.RLock()
	defer l.RUnlock()
	id, err := b.write(ctx, ws, r)
	if errors.Is(err, errNotFound) {
		b.mu.Lock()
		delete(b.ready, ws)
		b.mu.Unlock()
		id, err = b.write(ctx, ws, r)
	}
	return id, err
}

func (b *Backend) write(ctx context.Context, ws string, r memory.Record) (string, error) {
	if err := b.ensure(ctx, ws); err != nil {
		return "", err
	}
	gens, err := b.generations(ctx, ws)
	if err != nil {
		return "", err
	}
	k := key(r)
	gen := 1
	if g := gens[k]; len(g) > 0 {
		gen = g[len(g)-1]
	}
	s := sessionID(k, gen)
	if err := b.c.createSession(ctx, ws, s); err != nil {
		return "", err
	}
	msg := newMessage{Content: r.Text, PeerID: peerUser, CreatedAt: r.At.UTC(), Metadata: fields{
		Layer: r.Layer, Session: r.Session, Origin: r.Origin, Server: r.Server, Verdict: r.Decision.Verdict, Policy: r.Decision.Policy,
	}.metadata()}
	if r.Origin == content.KindModel {
		msg.PeerID = peerAgent
	}
	if r.Layer == memory.LongTerm {
		// A fact a program remembers is not material for the deriver.
		msg.Configuration = &configuration{Reasoning: reasoning{Enabled: false}}
	}
	got, err := b.c.addMessages(ctx, ws, s, []newMessage{msg})
	if err != nil {
		return "", err
	}
	if len(got) != 1 {
		return "", fmt.Errorf("honcho: add messages: %d messages back for one", len(got))
	}
	return "m-" + got[0].ID, nil
}

// read returns the records of every generation of the sessions under k in ws,
// each once, oldest first.
func (b *Backend) read(ctx context.Context, ws, subject, k string, gens []int) ([]memory.Record, error) {
	seen := map[string]bool{}
	var out []memory.Record
	for _, g := range gens {
		msgs, err := b.c.listMessages(ctx, ws, sessionID(k, g))
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
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
	ws, err := b.workspace(subject)
	if err != nil {
		return nil, err
	}
	gens, err := b.generations(ctx, ws)
	if err != nil {
		return nil, err
	}
	k := hex.EncodeToString([]byte(session))
	recs, err := b.read(ctx, ws, subject, k, gens[k])
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(recs, func(r memory.Record) bool { return r.Layer != memory.ShortTerm || r.At.Before(since) }), nil
}

// Recall returns at most limit facts written at or after since: the facts a
// program remembered and those the deriver formed about the user, matched by
// the service's semantic search, taking from each in turn so neither crowds
// the other out, and then remembered facts sharing a word with the query,
// which the search can miss until the service has embedded them; a fact whose
// text is the query comes first, and an empty query returns the newest.
func (b *Backend) Recall(ctx context.Context, subject, query string, limit int, since time.Time) ([]memory.Record, error) {
	ws, err := b.workspace(subject)
	if err != nil {
		return nil, err
	}
	gens, err := b.generations(ctx, ws)
	if err != nil {
		return nil, err
	}
	facts, err := b.read(ctx, ws, subject, factsKey, gens[factsKey])
	if err != nil {
		return nil, err
	}
	keep := func(r memory.Record) bool { return r.Layer == memory.LongTerm && !r.At.Before(since) }
	var found []memory.Record
	if query == "" {
		found = append(found, facts...)
		cs, err := b.c.listConclusions(ctx, ws)
		if err != nil && !errors.Is(err, errNotFound) {
			return nil, err
		}
		for _, c := range cs {
			found = append(found, derived(subject, c))
		}
		found = slices.DeleteFunc(found, func(r memory.Record) bool { return !keep(r) })
		slices.SortStableFunc(found, func(a, b memory.Record) int { return b.At.Compare(a.At) })
		return found[:min(limit, len(found))], nil
	}
	var searched []memory.Record
	for _, g := range gens[factsKey] {
		msgs, err := b.c.searchMessages(ctx, ws, sessionID(factsKey, g), query)
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			if r, ok := recordOf(subject, m); ok && keep(r) {
				searched = append(searched, r)
			}
		}
	}
	cs, err := b.c.queryConclusions(ctx, ws, query)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, err
	}
	var concluded []memory.Record
	for _, c := range cs {
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

// DeleteSubject deletes subject's workspace, with every record and every
// derived fact in it, waiting for the service to let it go.
func (b *Backend) DeleteSubject(ctx context.Context, subject string) error {
	ws, err := b.workspace(subject)
	if err != nil {
		return err
	}
	l := b.lock(ws)
	l.Lock()
	defer l.Unlock()
	if err := b.deleteWorkspace(ctx, ws); err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.ready, ws)
	b.mu.Unlock()
	return nil
}

// deleteWorkspace deletes every session of ws, then ws, retrying while the
// service still holds a session.
func (b *Backend) deleteWorkspace(ctx context.Context, ws string) error {
	ss, err := b.c.listSessions(ctx, ws)
	if errors.Is(err, errNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, s := range ss {
		if err := b.c.deleteSession(ctx, ws, s.ID); err != nil && !errors.Is(err, errNotFound) {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, b.deleteWait)
	defer cancel()
	pause := 50 * time.Millisecond
	for {
		err := b.c.deleteWorkspace(ctx, ws)
		switch {
		case err == nil, errors.Is(err, errNotFound):
			return nil
		case !errors.Is(err, errConflict):
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("honcho: the service kept the workspace's sessions: %w", err)
		case <-time.After(pause):
		}
		pause = min(2*pause, time.Second)
	}
}

// DeleteBefore deletes every record of the namespace written before t, and
// every derived fact formed before it.
func (b *Backend) DeleteBefore(ctx context.Context, t time.Time) error {
	all, err := b.c.listWorkspaces(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, w := range all {
		if !strings.HasPrefix(w.ID, b.scope()) {
			continue
		}
		if err := b.purge(ctx, w.ID, t); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// purge deletes what in ws is older than t.
func (b *Backend) purge(ctx context.Context, ws string, t time.Time) error {
	l := b.lock(ws)
	l.Lock()
	defer l.Unlock()
	cs, err := b.c.listConclusions(ctx, ws)
	if err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	for _, c := range cs {
		if c.CreatedAt.Before(t) {
			if err := b.c.deleteConclusion(ctx, ws, c.ID); err != nil && !errors.Is(err, errNotFound) {
				return err
			}
		}
	}
	gens, err := b.generations(ctx, ws)
	if err != nil {
		return err
	}
	var errs []error
	for k, g := range gens {
		if err := b.rewrite(ctx, ws, k, g, t); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// maxBatch is the most messages the service takes in one call.
const maxBatch = 100

// rewrite leaves the sessions under k holding only the records written at or
// after t. A message bonyan did not write is not a record, and is not kept. When every record is older, every generation is deleted. When some
// are, or an earlier rewrite was left half done, the newer records go to a new
// generation first, and the older generations are deleted only once it holds
// them all, so a rewrite cut short loses nothing.
func (b *Backend) rewrite(ctx context.Context, ws, k string, gens []int, t time.Time) error {
	var all []message
	for _, g := range gens {
		msgs, err := b.c.listMessages(ctx, ws, sessionID(k, g))
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
		if !ok || seen[r.ID] {
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
		next := sessionID(k, gens[len(gens)-1]+1)
		if err := b.c.createSession(ctx, ws, next); err != nil {
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
			got, err := b.c.addMessages(ctx, ws, next, chunk)
			if err != nil {
				return err
			}
			if len(got) != len(chunk) {
				return fmt.Errorf("honcho: add messages: %d of %d copied", len(got), len(chunk))
			}
		}
	}
	for _, g := range gens {
		if err := b.c.deleteSession(ctx, ws, sessionID(k, g)); err != nil && !errors.Is(err, errNotFound) {
			return err
		}
	}
	return nil
}
