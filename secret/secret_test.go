package secret_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/secret"
)

// countingSource serves fixed values and counts every lookup.
type countingSource struct {
	mu      sync.Mutex
	values  map[string]string
	lookups int
}

func (c *countingSource) Lookup(_ context.Context, name string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lookups++
	v, ok := c.values[name]
	if !ok {
		return "", secret.ErrNotFound
	}
	return v, nil
}

func TestScopedResolvesOnlyItsGrant(t *testing.T) {
	src := &countingSource{values: map[string]string{"mail_token": "tok-1", "db_password": "pw-1"}}
	r := secret.NewResolver(src)
	scoped := r.Scope("mail_token")

	v, err := scoped.Resolve(context.Background(), "mail_token")
	require.NoError(t, err)
	assert.Equal(t, "tok-1", v.Reveal())

	_, err = scoped.Resolve(context.Background(), "db_password")
	require.ErrorIs(t, err, secret.ErrNotGranted)
	assert.Equal(t, 1, src.lookups, "a name outside the grant is refused before any source is read")
	assert.Equal(t, "db_password is pw-1", r.Scrubber().Scrub("db_password is pw-1"), "a refused value is never resolved")

	_, err = r.Scope().Resolve(context.Background(), "mail_token")
	require.ErrorIs(t, err, secret.ErrNotGranted, "an empty grant reaches nothing")
}

func TestSourcesAreTriedInOrder(t *testing.T) {
	first := &countingSource{values: map[string]string{"a": "from-first"}}
	second := &countingSource{values: map[string]string{"a": "from-second", "b": "only-second"}}
	s := secret.NewResolver(first, second).Scope("a", "b", "c")
	ctx := context.Background()

	v, err := s.Resolve(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, "from-first", v.Reveal())

	v, err = s.Resolve(ctx, "b")
	require.NoError(t, err)
	assert.Equal(t, "only-second", v.Reveal())

	_, err = s.Resolve(ctx, "c")
	require.ErrorIs(t, err, secret.ErrNotFound)
}

type failingSource struct{ err error }

func (f failingSource) Lookup(context.Context, string) (string, error) { return "", f.err }

type panickingSource struct{}

func (panickingSource) Lookup(context.Context, string) (string, error) { panic("source broke") }

func TestSourceFailureEndsTheLookup(t *testing.T) {
	boom := errors.New("store unreachable")
	later := &countingSource{values: map[string]string{"a": "later"}}

	_, err := secret.NewResolver(failingSource{err: boom}, later).Scope("a").Resolve(context.Background(), "a")
	require.ErrorIs(t, err, boom)
	assert.Zero(t, later.lookups, "a failing source is not skipped")

	_, err = secret.NewResolver(panickingSource{}, later).Scope("a").Resolve(context.Background(), "a")
	require.Error(t, err)
	assert.Zero(t, later.lookups)
}

func TestValueNeverPrints(t *testing.T) {
	v, err := secret.NewResolver(&countingSource{values: map[string]string{"k": "hunter2"}}).Scope("k").Resolve(context.Background(), "k")
	require.NoError(t, err)

	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		assert.Equal(t, secret.Redacted, fmt.Sprintf(verb, v), verb)
	}
	assert.Equal(t, secret.Redacted, v.String())
	assert.NotContains(t, fmt.Sprintf("%+v", struct{ V secret.Value }{v}), "hunter2")

	b, err := json.Marshal(map[string]any{"v": v})
	require.NoError(t, err)
	assert.JSONEq(t, `{"v":"[REDACTED]"}`, string(b))

	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("resolved", "v", v)
	assert.NotContains(t, buf.String(), "hunter2")
	assert.Contains(t, buf.String(), secret.Redacted)
}

func TestResolvedValueIsScrubbedFromToolOutput(t *testing.T) {
	r := secret.NewResolver(&countingSource{values: map[string]string{"api_key": "key-9f8e7d"}})
	_, err := r.Scope("api_key").Resolve(context.Background(), "api_key")
	require.NoError(t, err)

	msg := model.ToolResult("call-1", `{"echo":"Authorization: Bearer key-9f8e7d"}`)
	for i, p := range msg.Parts {
		msg.Parts[i] = r.Scrubber().ScrubText(p)
	}

	u, ok := msg.Parts[0].(content.Untrusted)
	require.True(t, ok, "tool output stays untrusted")
	assert.Equal(t, `{"echo":"Authorization: Bearer [REDACTED]"}`, u.Raw())
	assert.Equal(t, content.Provenance{Kind: content.KindTool, ID: "call-1"}, u.Provenance())

	tr, ok := r.Scrubber().ScrubText(content.Instruction("use key-9f8e7d")).(content.Trusted)
	require.True(t, ok, "trusted text stays trusted")
	assert.Equal(t, "use [REDACTED]", tr.String())

	mem := content.Provenance{Kind: content.KindMemory, Origin: content.KindUser}
	tr, ok = r.Scrubber().ScrubText(content.TrustedFrom(mem, "use key-9f8e7d")).(content.Trusted)
	require.True(t, ok)
	assert.Equal(t, "use [REDACTED]", tr.String())
	assert.Equal(t, mem, tr.Provenance(), "scrubbing keeps where trusted text came from")
}

type stringer struct{ s string }

func (s stringer) String() string { return s.s }

func TestResolvedValueIsScrubbedFromLogOutput(t *testing.T) {
	r := secret.NewResolver(&countingSource{values: map[string]string{"pw": "correct-horse"}})
	var buf bytes.Buffer
	log := slog.New(r.Scrubber().Handler(slog.NewJSONHandler(&buf, nil)))

	// Bound before the value is resolved: still scrubbed when the record is
	// handled.
	bound := log.With("conn", "db://app:correct-horse@db").WithGroup("req")

	_, err := r.Scope("pw").Resolve(context.Background(), "pw")
	require.NoError(t, err)

	log.Info("login with correct-horse",
		"plain", "correct-horse",
		"err", errors.New("auth failed for correct-horse"),
		"obj", stringer{"pw=correct-horse"},
		slog.Group("g", "inner", "x correct-horse y"),
		"count", 3,
	)
	bound.Info("query", "q", "select correct-horse")

	out := buf.String()
	assert.NotContains(t, out, "correct-horse")
	assert.Contains(t, out, `"msg":"login with [REDACTED]"`)
	assert.Contains(t, out, `"count":3`, "an attribute holding no secret keeps its type")
	assert.Contains(t, out, `"conn":"db://app:[REDACTED]@db"`)
	assert.Contains(t, out, `"req":{"q":"select [REDACTED]"}`, "groups survive scrubbing")
}

// A secret whose value is a number is scrubbed when it is logged as a number,
// and so is every other kind a value can be logged as.
func TestNonStringAttributesAreScrubbed(t *testing.T) {
	r := secret.NewResolver(&countingSource{values: map[string]string{
		"pin": "4711", "rate": "0.75", "flag": "true", "wait": "1m30s",
	}})
	s := r.Scope("pin", "rate", "flag", "wait")
	for _, n := range []string{"pin", "rate", "flag", "wait"} {
		_, err := s.Resolve(context.Background(), n)
		require.NoError(t, err)
	}
	var buf bytes.Buffer
	log := slog.New(r.Scrubber().Handler(slog.NewTextHandler(&buf, nil)))

	log.Info("x", "pin", 4711, "upin", uint64(4711), "rate", 0.75, "flag", true, "wait", 90*time.Second, "other", 12)

	out := buf.String()
	for _, leaked := range []string{"4711", "0.75", "true", "1m30s"} {
		assert.NotContains(t, out, leaked)
	}
	assert.Contains(t, out, "pin=[REDACTED]")
	assert.Contains(t, out, "other=12", "a number holding no secret is kept")
}

func TestRotationIsPickedUpAndOldValueStaysScrubbed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(path, []byte("old-value\n"), 0o600))
	t.Setenv("BONYAN_TEST_ENV_TOKEN", "env-old")

	r := secret.NewResolver(secret.Dir{Path: dir}, secret.Env{})
	s := r.Scope("token", "BONYAN_TEST_ENV_TOKEN")
	ctx := context.Background()

	v, err := s.Resolve(ctx, "token")
	require.NoError(t, err)
	assert.Equal(t, "old-value", v.Reveal(), "one trailing newline is removed")
	v, err = s.Resolve(ctx, "BONYAN_TEST_ENV_TOKEN")
	require.NoError(t, err)
	assert.Equal(t, "env-old", v.Reveal())

	require.NoError(t, os.WriteFile(path, []byte("new-value\r\n"), 0o600))
	t.Setenv("BONYAN_TEST_ENV_TOKEN", "env-new")

	v, err = s.Resolve(ctx, "token")
	require.NoError(t, err)
	assert.Equal(t, "new-value", v.Reveal())
	v, err = s.Resolve(ctx, "BONYAN_TEST_ENV_TOKEN")
	require.NoError(t, err)
	assert.Equal(t, "env-new", v.Reveal())

	assert.Equal(t, "[REDACTED] [REDACTED] [REDACTED] [REDACTED]",
		r.Scrubber().Scrub("old-value new-value env-old env-new"))
}

func TestDirSource(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), []byte("A"), 0o600))
	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(outside, []byte("X"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "escape")))
	d := secret.Dir{Path: dir}
	ctx := context.Background()

	v, err := d.Lookup(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, "A", v, "a value with no trailing newline is kept whole")

	_, err = d.Lookup(ctx, "missing")
	require.ErrorIs(t, err, secret.ErrNotFound)

	for _, bad := range []string{"", ".", "..", "../a", "sub/a", `sub\a`, "a\x00"} {
		_, err := d.Lookup(ctx, bad)
		require.Error(t, err, "%q", bad)
		assert.NotErrorIs(t, err, secret.ErrNotFound, "%q is refused, not looked up", bad)
	}

	_, err = d.Lookup(ctx, "escape")
	require.Error(t, err, "a link out of the directory is not followed")
	assert.NotErrorIs(t, err, secret.ErrNotFound)
}

func TestEnvSource(t *testing.T) {
	t.Setenv("BONYAN_TEST_SET", "v")
	v, err := secret.Env{}.Lookup(context.Background(), "BONYAN_TEST_SET")
	require.NoError(t, err)
	assert.Equal(t, "v", v)

	_, err = secret.Env{}.Lookup(context.Background(), "BONYAN_TEST_SURELY_UNSET")
	require.ErrorIs(t, err, secret.ErrNotFound)
}

func TestScrubbing(t *testing.T) {
	r := secret.NewResolver(&countingSource{values: map[string]string{
		"short": "abc", "long": "abcdef", "empty": "", "pin": "1",
	}})
	s := r.Scope("short", "long", "empty", "pin")
	sc := r.Scrubber()
	assert.Equal(t, "abcdef abc", sc.Scrub("abcdef abc"), "nothing is scrubbed before a value is resolved")

	for _, n := range []string{"short", "long", "empty"} {
		_, err := s.Resolve(context.Background(), n)
		require.NoError(t, err)
	}
	assert.Equal(t, "[REDACTED] [REDACTED]x", sc.Scrub("abcdef abcx"), "a value containing another is replaced whole")
	assert.Equal(t, "plain text", sc.Scrub("plain text"), "an empty value is never scrubbed")

	// A short value is redacted everywhere it occurs, including in text that
	// has nothing to do with it. This is the documented, safe direction.
	_, err := s.Resolve(context.Background(), "pin")
	require.NoError(t, err)
	assert.Equal(t, "room [REDACTED]0[REDACTED], [REDACTED] item", sc.Scrub("room 101, 1 item"))
}

func TestScrubberIsSafeForConcurrentUse(t *testing.T) {
	values := map[string]string{}
	for i := range 50 {
		values[fmt.Sprint("n", i)] = fmt.Sprint("value-", i, "-x")
	}
	r := secret.NewResolver(&countingSource{values: values})
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = r.Scope(fmt.Sprint("n", i)).Resolve(context.Background(), fmt.Sprint("n", i))
		}()
		go func() {
			defer wg.Done()
			_ = r.Scrubber().Scrub("value-1-x value-2-x")
		}()
	}
	wg.Wait()
	assert.Equal(t, "[REDACTED] [REDACTED]", r.Scrubber().Scrub("value-1-x value-49-x"))
}

func TestASectionIsScrubbedItemByItem(t *testing.T) {
	r := secret.NewResolver(&countingSource{values: map[string]string{"api_key": "key-9f8e7d"}})
	_, err := r.Scope("api_key").Resolve(context.Background(), "api_key")
	require.NoError(t, err)
	src := content.Provenance{Kind: content.KindTool, ID: "call-1"}
	out, ok := r.Scrubber().ScrubText(content.NewSection("tool result", content.From(src, "Bearer key-9f8e7d"))).(content.Section)
	require.True(t, ok, "a section stays a section")
	assert.Equal(t, "tool result", out.Label())
	require.Len(t, out.Items(), 1)
	assert.Equal(t, "Bearer [REDACTED]", out.Items()[0].Raw())
	assert.Equal(t, src, out.Items()[0].Provenance())
}
