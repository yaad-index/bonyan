//go:build unix

package dirstore_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/internal/dirstore"
)

// A store is owner-only: it is made so, and one open to others is refused.
func TestAStoreIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s")
	_, err := dirstore.Open(path)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	open := filepath.Join(t.TempDir(), "open")
	require.NoError(t, os.Mkdir(open, 0o755))
	require.NoError(t, os.Chmod(open, 0o755))
	_, err = dirstore.Open(open)
	require.Error(t, err)
	_, err = dirstore.Open("")
	require.Error(t, err)
}

// Files are written whole, read back, listed by name and deleted; the lock
// and a write's temporary file are never listed, and no temporary file is
// left behind.
func TestFilesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s")
	d, err := dirstore.Open(path)
	require.NoError(t, err)
	require.NoError(t, d.Locked(func(tx dirstore.Tx) error {
		require.NoError(t, tx.Put("a", map[string]int{"n": 1}))
		require.NoError(t, tx.Put("a", map[string]int{"n": 2}))
		require.NoError(t, tx.Put("b", map[string]int{"n": 3}))
		var got map[string]int
		require.NoError(t, tx.Get("a", &got))
		assert.Equal(t, 2, got["n"], "replaced")
		require.ErrorIs(t, tx.Get("missing", &got), dirstore.ErrNotFound)
		names, err := tx.Names()
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"a", "b"}, names)
		require.NoError(t, tx.Delete("a"))
		require.NoError(t, tx.Delete("a"), "gone already")
		names, err = tx.Names()
		require.NoError(t, err)
		assert.Equal(t, []string{"b"}, names)
		for _, bad := range []string{"", ".lock", "../x", "a/b"} {
			require.Error(t, tx.Put(bad, 1), bad)
		}
		return nil
	}))
	entries, err := os.ReadDir(path)
	require.NoError(t, err)
	var files []string
	for _, e := range entries {
		files = append(files, e.Name())
		info, err := e.Info()
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), e.Name())
	}
	assert.ElementsMatch(t, []string{".lock", "b.json"}, files, "no temporary file left")
}

// The lock lets one operation in at a time, also between handles on the
// same directory: increments under it are never lost.
func TestTheLockExcludes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s")
	var wg sync.WaitGroup
	for range 8 {
		d, err := dirstore.Open(path)
		require.NoError(t, err)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				assert.NoError(t, d.Locked(func(tx dirstore.Tx) error {
					var n int
					if err := tx.Get("n", &n); err != nil && err != dirstore.ErrNotFound {
						return err
					}
					return tx.Put("n", n+1)
				}))
			}
		}()
	}
	wg.Wait()
	d, err := dirstore.Open(path)
	require.NoError(t, err)
	var n int
	require.NoError(t, d.Locked(func(tx dirstore.Tx) error { return tx.Get("n", &n) }))
	assert.Equal(t, 80, n)
}

// A temporary file a crashed write left behind is removed by the next
// operation, so what it held does not outlive a deletion.
func TestALeftoverTemporaryFileIsRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s")
	d, err := dirstore.Open(path)
	require.NoError(t, err)
	left := filepath.Join(path, ".tmp-123")
	require.NoError(t, os.WriteFile(left, []byte(`{"subject":"ana"}`), 0o600))
	require.NoError(t, d.Locked(func(tx dirstore.Tx) error {
		_, err := os.Stat(left)
		assert.ErrorIs(t, err, os.ErrNotExist, "gone before the operation runs")
		return nil
	}))
}
