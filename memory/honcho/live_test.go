package honcho_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/memory/honcho"
	"github.com/yaad-index/bonyan/memory/memorytest"
	"github.com/yaad-index/bonyan/trust"
)

// livePrefix starts every workspace the live tests make, so they can be told
// from any other on the service and removed after.
const livePrefix = "bonyan-livetest"

// liveURL is the running service the live tests use, from
// BONYAN_HONCHO_URL; without it they are skipped. BONYAN_HONCHO_TOKEN, when
// the service requires one, must reach every workspace: the tests make and
// remove workspaces of their own. They run with the deriver off, so they make
// no model calls beyond the service's embeddings.
func liveURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("BONYAN_HONCHO_URL")
	if u == "" {
		t.Skip("BONYAN_HONCHO_URL is not set")
	}
	t.Cleanup(func() { removeLive(t, u) })
	return u
}

// removeLive deletes every workspace the live tests made, sessions first.
func removeLive(t *testing.T, url string) {
	t.Helper()
	call := func(method, path string, body any) (*http.Response, error) {
		b, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(context.Background(), method, url+path, strings.NewReader(string(b)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if tok := os.Getenv("BONYAN_HONCHO_TOKEN"); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		return http.DefaultClient.Do(req)
	}
	ids := func(path string) []string {
		resp, err := call(http.MethodPost, path+"?size=100", map[string]any{})
		if err != nil {
			t.Errorf("live cleanup: %v", err)
			return nil
		}
		defer func() { _ = resp.Body.Close() }()
		var p struct{ Items []struct{ ID string } }
		_ = json.NewDecoder(resp.Body).Decode(&p)
		var out []string
		for _, it := range p.Items {
			out = append(out, it.ID)
		}
		return out
	}
	for _, ws := range ids("/v3/workspaces/list") {
		if !strings.HasPrefix(ws, livePrefix+"-") {
			continue
		}
		for _, s := range ids("/v3/workspaces/" + ws + "/sessions/list") {
			if resp, err := call(http.MethodDelete, "/v3/workspaces/"+ws+"/sessions/"+s, nil); err == nil {
				_ = resp.Body.Close()
			}
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			resp, err := call(http.MethodDelete, "/v3/workspaces/"+ws, nil)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusConflict {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Errorf("live cleanup: workspace %s was not deleted", ws)
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// The conformance suite, against the running service.
func TestLiveConformance(t *testing.T) {
	url := liveURL(t)
	run := time.Now().UnixNano()
	n := 0
	memorytest.Run(t, func(*testing.T) memorytest.Open {
		// The service keeps what one test wrote, so each test gets a storage
		// of its own: workspaces under a prefix no other test uses.
		n++
		prefix := livePrefix + "-" + strconv.FormatInt(run, 36) + "-" + strconv.Itoa(n)
		return func(namespace string) memory.Backend {
			b, err := honcho.Open(honcho.Options{URL: url, Namespace: namespace, Prefix: prefix, Token: os.Getenv("BONYAN_HONCHO_TOKEN")})
			require.NoError(t, err)
			return b
		}
	})
}

// A token the service scopes to one workspace is enough for every operation
// (ADR 0003 §5): run with BONYAN_HONCHO_WORKSPACE naming a workspace under the
// live tests' prefix and BONYAN_HONCHO_SCOPED_TOKEN a token scoped to it.
// Peers cannot be deleted, so the workspace is the live tests' own, never a
// program's: the test refuses any other, and removing the workspace after it
// takes BONYAN_HONCHO_TOKEN.
func TestLiveScopedToken(t *testing.T) {
	url := liveURL(t)
	ws, token := os.Getenv("BONYAN_HONCHO_WORKSPACE"), os.Getenv("BONYAN_HONCHO_SCOPED_TOKEN")
	if ws == "" || token == "" {
		t.Skip("BONYAN_HONCHO_WORKSPACE and BONYAN_HONCHO_SCOPED_TOKEN are not set")
	}
	require.True(t, strings.HasPrefix(ws, livePrefix+"-"), "the workspace must be the live tests' own, under %s-", livePrefix)
	b, err := honcho.Open(honcho.Options{URL: url, Namespace: "livetest", Workspace: ws, Token: token, DeleteWait: time.Minute})
	require.NoError(t, err)

	run := strconv.FormatInt(time.Now().UnixNano(), 36)
	ana, bo := "ana-"+run, "bo-"+run
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, subject := range []string{ana, bo} {
		for _, r := range []memory.Record{
			{Layer: memory.ShortTerm, Subject: subject, Session: "s1", Origin: content.KindUser, Text: subject + " old event", At: now.Add(-2 * time.Hour)},
			{Layer: memory.ShortTerm, Subject: subject, Session: "s1", Origin: content.KindUser, Text: subject + " new event", At: now},
			{Layer: memory.LongTerm, Subject: subject, Origin: content.KindUser, Text: subject + " rides trains", At: now},
		} {
			r.Decision = memory.Decision{Verdict: trust.Untrusted, Policy: "default"}
			_, err := b.Write(ctx, r)
			require.NoError(t, err, "write")
		}
	}
	for _, subject := range []string{ana, bo} {
		got, err := b.Recall(ctx, subject, "", 10, time.Time{})
		require.NoError(t, err, "recall")
		assert.Equal(t, []string{subject + " rides trains"}, texts(got), "recall keeps users apart")
	}

	require.NoError(t, b.DeleteBefore(ctx, now.Add(-time.Hour)), "retention, without listing workspaces")
	h, err := b.History(ctx, ana, "s1", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{ana + " new event"}, texts(h))

	require.NoError(t, b.DeleteSubject(ctx, ana), "erase")
	h, err = b.History(ctx, ana, "s1", time.Time{})
	require.NoError(t, err)
	assert.Empty(t, h)
	got, err := b.Recall(ctx, ana, "", 10, time.Time{})
	require.NoError(t, err)
	assert.Empty(t, got)
	h, err = b.History(ctx, bo, "s1", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{bo + " new event"}, texts(h), "the other user is untouched")
	require.NoError(t, b.DeleteSubject(ctx, bo))
}
