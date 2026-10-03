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

	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/memory/honcho"
	"github.com/yaad-index/bonyan/memory/memorytest"
)

// livePrefix starts every workspace the live tests make, so they can be told
// from any other on the service and removed after.
const livePrefix = "bonyan-livetest"

// liveURL is the running service the live tests use, from
// BONYAN_HONCHO_URL; without it they are skipped. They run with the deriver
// off, so they make no model calls beyond the service's embeddings.
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
			b, err := honcho.Open(honcho.Options{URL: url, Namespace: namespace, Prefix: prefix})
			require.NoError(t, err)
			return b
		}
	})
}
