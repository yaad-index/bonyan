package honcho_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
)

// fake is the part of the service's v3 API the backend uses, held in memory.
// Its search ranks every message by the words it shares with the query, as a
// semantic search returns near matches too. Lists come in pages of three, so
// a client that reads one page only misses records.
type fake struct {
	mu  sync.Mutex
	ws  map[string]*fakeWorkspace
	ids int
	now time.Time

	// fail, when set, answers a request with 500 when it returns true.
	fail func(method, path string) bool
	// busy answers this many workspace deletes with 409 first.
	busy int
	// unembedded hides every message from search, as the service does until
	// it has embedded them.
	unembedded bool
	// short answers a batch of messages with one fewer than it took.
	short bool
	// scope, when set, answers 401 to any call outside that one workspace,
	// listing workspaces included, as a token scoped to it does.
	scope string
	// leaky ignores the filters of a conclusion list or query and lists every
	// session of the workspace as any peer's, so a backend relying on the
	// service alone to keep users apart shows it.
	leaky bool
	// queue is the deriver's work by status filter, "sender_id=<peer>" or
	// "observer_id=<peer>". Each status read with work takes one unit off,
	// and when the last unit goes, late runs, as a derivation finishing after
	// an erase began. busyAs reports the work as under way rather than
	// waiting.
	queue  map[string]int
	late   func(ws, peer string)
	busyAs string
	// filters records the filters of every conclusion list or query.
	filters []map[string]any
	// calls records every request as "METHOD path".
	calls []string
}

type fakeWorkspace struct {
	config      map[string]any
	metadata    map[string]any
	peers       map[string]bool
	peerMeta    map[string]map[string]any
	cards       map[string][]any
	sessions    map[string]*fakeSession
	order       []string
	conclusions []fakeConclusion
}

type fakeSession struct {
	peers    map[string]any
	messages []map[string]any
}

type fakeConclusion struct {
	ID, Content, Observer, Observed string
	At                              time.Time
}

const fakePage = 3

func newFake(t *testing.T) (*fake, *httptest.Server) {
	t.Helper()
	f := &fake{ws: map[string]*fakeWorkspace{}, queue: map[string]int{}, now: time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fake) id() string {
	f.ids++
	return "id" + strconv.Itoa(f.ids)
}

func write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func paged[T any](w http.ResponseWriter, r *http.Request, items []T) {
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	n = max(n, 1)
	pages := (len(items) + fakePage - 1) / fakePage
	from := min((n-1)*fakePage, len(items))
	to := min(from+fakePage, len(items))
	write(w, http.StatusOK, map[string]any{"items": items[from:to], "page": n, "pages": pages, "total": len(items)})
}

func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func shared(a, b string) int {
	n := 0
	wb := words(b)
	for _, w := range words(a) {
		if slices.Contains(wb, w) {
			n++
		}
	}
	return n
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil && f.fail(r.Method, r.URL.Path) {
		write(w, http.StatusInternalServerError, map[string]string{"detail": "SECRET-BODY failure"})
		return
	}
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/v3/workspaces"), "/")
	// p[0] is empty; p[1] the workspace, or "list"; then the resource.
	if f.scope != "" {
		target := ""
		if len(p) > 1 {
			target = p[1]
		} else if id, ok := body["id"].(string); ok {
			target = id
		}
		if target != f.scope {
			write(w, http.StatusUnauthorized, nil)
			return
		}
	}
	switch {
	case len(p) == 1 && r.Method == http.MethodPost:
		id := body["id"].(string)
		if ws, ok := f.ws[id]; ok {
			write(w, http.StatusOK, map[string]any{"id": id, "metadata": ws.metadata})
			return
		}
		cfg, _ := body["configuration"].(map[string]any)
		meta, _ := body["metadata"].(map[string]any)
		f.ws[id] = newFakeWorkspace(cfg, meta)
		write(w, http.StatusOK, map[string]any{"id": id, "metadata": meta})
		return
	case len(p) == 2 && p[1] == "list":
		ids := make([]map[string]any, 0, len(f.ws))
		for id := range f.ws {
			ids = append(ids, map[string]any{"id": id})
		}
		slices.SortFunc(ids, func(a, b map[string]any) int { return strings.Compare(a["id"].(string), b["id"].(string)) })
		paged(w, r, ids)
		return
	}
	ws, ok := f.ws[p[1]]
	if !ok {
		write(w, http.StatusNotFound, nil)
		return
	}
	switch {
	case len(p) == 2 && r.Method == http.MethodPut:
		ws.metadata, _ = body["metadata"].(map[string]any)
		ws.config, _ = body["configuration"].(map[string]any)
		write(w, http.StatusOK, map[string]any{"id": p[1], "metadata": ws.metadata})
	case len(p) == 4 && p[2] == "peers" && r.Method == http.MethodPut:
		if !ws.peers[p[3]] {
			write(w, http.StatusNotFound, nil)
			return
		}
		ws.peerMeta[p[3]], _ = body["metadata"].(map[string]any)
		write(w, http.StatusOK, map[string]any{"id": p[3]})
	case len(p) == 5 && p[2] == "peers" && p[4] == "card":
		if !ws.peers[p[3]] {
			write(w, http.StatusNotFound, nil)
			return
		}
		card, _ := body["peer_card"].([]any)
		ws.cards[p[3]+"|"+r.URL.Query().Get("target")] = card
		write(w, http.StatusOK, map[string]any{"peer_card": card})
	case len(p) == 5 && p[2] == "peers" && p[4] == "sessions":
		var out []map[string]any
		for _, id := range ws.order {
			s, ok := ws.sessions[id]
			if !ok {
				continue
			}
			if _, member := s.peers[p[3]]; member || f.leaky {
				out = append(out, map[string]any{"id": id})
			}
		}
		paged(w, r, out)
	case len(p) == 4 && p[2] == "queue" && p[3] == "status":
		q := r.URL.Query()
		filter, peer := "sender_id="+q.Get("sender_id"), q.Get("sender_id")
		if peer == "" {
			filter, peer = "observer_id="+q.Get("observer_id"), q.Get("observer_id")
		}
		n := f.queue[filter]
		if n > 0 {
			f.queue[filter] = n - 1
			if n == 1 && f.late != nil {
				f.late(p[1], peer)
			}
		}
		status := map[string]any{"pending_work_units": n, "in_progress_work_units": 0}
		if f.busyAs == "in_progress" {
			status = map[string]any{"pending_work_units": 0, "in_progress_work_units": n}
		}
		write(w, http.StatusOK, status)
	case len(p) == 2 && r.Method == http.MethodDelete:
		if len(ws.sessions) > 0 || f.busy > 0 {
			f.busy = max(f.busy-1, 0)
			write(w, http.StatusConflict, nil)
			return
		}
		delete(f.ws, p[1])
		write(w, http.StatusAccepted, nil)
	case len(p) == 3 && p[2] == "peers":
		code := http.StatusOK
		if !ws.peers[body["id"].(string)] {
			ws.peers[body["id"].(string)], code = true, http.StatusCreated
		}
		write(w, code, map[string]any{"id": body["id"]})
	case len(p) == 3 && p[2] == "sessions":
		id := body["id"].(string)
		if _, ok := ws.sessions[id]; ok {
			write(w, http.StatusOK, map[string]any{"id": id})
			return
		}
		peers, _ := body["peers"].(map[string]any)
		for peer := range peers {
			if !ws.peers[peer] {
				write(w, http.StatusNotFound, map[string]string{"detail": "peer not found"})
				return
			}
		}
		ws.sessions[id] = &fakeSession{peers: peers}
		ws.order = append(ws.order, id)
		write(w, http.StatusCreated, map[string]any{"id": id})
	case len(p) == 4 && p[2] == "sessions" && p[3] == "list":
		var out []map[string]any
		for _, id := range ws.order {
			if _, ok := ws.sessions[id]; ok {
				out = append(out, map[string]any{"id": id})
			}
		}
		paged(w, r, out)
	case len(p) == 4 && p[2] == "sessions" && r.Method == http.MethodDelete:
		if _, ok := ws.sessions[p[3]]; !ok {
			write(w, http.StatusNotFound, nil)
			return
		}
		delete(ws.sessions, p[3])
		write(w, http.StatusAccepted, nil)
	case len(p) == 5 && p[2] == "sessions":
		s, ok := ws.sessions[p[3]]
		if !ok {
			write(w, http.StatusNotFound, nil)
			return
		}
		switch p[4] {
		case "messages":
			batch := body["messages"].([]any)
			if len(batch) > 100 {
				write(w, http.StatusUnprocessableEntity, map[string]string{"detail": "too many messages"})
				return
			}
			var out []map[string]any
			for _, m := range batch {
				m := m.(map[string]any)
				m["id"] = f.id()
				if _, ok := m["metadata"]; !ok {
					m["metadata"] = map[string]any{}
				}
				s.messages = append(s.messages, m)
				out = append(out, m)
			}
			if f.short && len(out) > 0 {
				out = out[:len(out)-1]
			}
			write(w, http.StatusCreated, out)
		case "search":
			query := body["query"].(string)
			ranked := slices.Clone(s.messages)
			if f.unembedded {
				ranked = nil
			}
			slices.SortStableFunc(ranked, func(a, b map[string]any) int {
				return shared(query, b["content"].(string)) - shared(query, a["content"].(string))
			})
			limit := int(body["limit"].(float64))
			write(w, http.StatusOK, ranked[:min(limit, len(ranked))])
		default:
			write(w, http.StatusNotFound, nil)
		}
	case len(p) == 6 && p[2] == "sessions" && p[4] == "messages" && p[5] == "list":
		s, ok := ws.sessions[p[3]]
		if !ok {
			write(w, http.StatusNotFound, nil)
			return
		}
		paged(w, r, s.messages)
	case len(p) == 3 && p[2] == "conclusions":
		var out []map[string]any
		for _, c := range body["conclusions"].([]any) {
			c := c.(map[string]any)
			fc := fakeConclusion{ID: f.id(), Content: c["content"].(string), Observer: c["observer_id"].(string), Observed: c["observed_id"].(string), At: f.now}
			ws.conclusions = append(ws.conclusions, fc)
			out = append(out, conclusionJSON(fc))
		}
		write(w, http.StatusCreated, out)
	case len(p) == 4 && p[2] == "conclusions" && (p[3] == "list" || p[3] == "query"):
		filters, _ := body["filters"].(map[string]any)
		f.filters = append(f.filters, filters)
		if p[3] == "query" && (filters["observer_id"] == nil || filters["observed_id"] == nil) {
			write(w, http.StatusUnprocessableEntity, map[string]string{"detail": "observer and observed must be specified for semantic search"})
			return
		}
		var out []map[string]any
		for _, c := range ws.conclusions {
			if f.leaky || (filters["observer_id"] == nil || filters["observer_id"] == c.Observer) && (filters["observed_id"] == nil || filters["observed_id"] == c.Observed) {
				out = append(out, conclusionJSON(c))
			}
		}
		if p[3] == "list" {
			paged(w, r, out)
			return
		}
		query := body["query"].(string)
		slices.SortStableFunc(out, func(a, b map[string]any) int {
			return shared(query, b["content"].(string)) - shared(query, a["content"].(string))
		})
		limit := int(body["top_k"].(float64))
		write(w, http.StatusOK, out[:min(limit, len(out))])
	case len(p) == 4 && p[2] == "conclusions" && r.Method == http.MethodDelete:
		i := slices.IndexFunc(ws.conclusions, func(c fakeConclusion) bool { return c.ID == p[3] })
		if i < 0 {
			write(w, http.StatusNotFound, nil)
			return
		}
		ws.conclusions = slices.Delete(ws.conclusions, i, i+1)
		write(w, http.StatusNoContent, nil)
	default:
		write(w, http.StatusNotFound, nil)
	}
}

func newFakeWorkspace(cfg, meta map[string]any) *fakeWorkspace {
	return &fakeWorkspace{
		config: cfg, metadata: meta, peers: map[string]bool{}, peerMeta: map[string]map[string]any{},
		cards: map[string][]any{}, sessions: map[string]*fakeSession{},
	}
}

// makeWorkspace adds a workspace with metadata, as an operator would.
func (f *fake) makeWorkspace(id string, meta map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ws[id] = newFakeWorkspace(nil, meta)
}

// setCard sets a card in a workspace, as the service's deriver would.
func (f *fake) setCard(ws, observer, target string, card ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ws[ws].cards[observer+"|"+target] = card
}

// setPeerMeta sets a peer's metadata, as anyone able to write to the service
// could.
func (f *fake) setPeerMeta(ws, peer string, meta map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ws[ws].peerMeta[peer] = meta
}

// conclusionsOf lists the contents of a workspace's conclusions that peer
// holds or is the subject of.
func (f *fake) conclusionsOf(ws, peer string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.ws[ws].conclusions {
		if c.Observer == peer || c.Observed == peer {
			out = append(out, c.Content)
		}
	}
	return out
}

// called lists the requests the fake answered.
func (f *fake) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func conclusionJSON(c fakeConclusion) map[string]any {
	return map[string]any{"id": c.ID, "content": c.Content, "observer_id": c.Observer, "observed_id": c.Observed, "created_at": c.At.Format(time.RFC3339Nano)}
}

// workspaces lists the fake's workspaces.
func (f *fake) workspaces() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id := range f.ws {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// sessions lists a workspace's sessions.
func (f *fake) sessions(ws string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id := range f.ws[ws].sessions {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// messages returns the messages of a workspace's session as stored.
func (f *fake) messages(ws, s string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ws[ws].sessions[s].messages)
}

// conclude adds a conclusion to a workspace, as the deriver would.
func (f *fake) conclude(ws, observer, observed, text string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.ws[ws]
	w.conclusions = append(w.conclusions, fakeConclusion{ID: f.id(), Content: text, Observer: observer, Observed: observed, At: at})
}

// plant adds a message to a workspace's session directly, as anyone able to
// write to the service could.
func (f *fake) plant(ws, s string, m map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m["id"] = f.id()
	f.ws[ws].sessions[s].messages = append(f.ws[ws].sessions[s].messages, m)
}
