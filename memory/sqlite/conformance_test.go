package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/memory/memorytest"
	"github.com/yaad-index/bonyan/memory/sqlite"
)

func TestConformance(t *testing.T) {
	memorytest.Run(t, func(t *testing.T) memory.Backend {
		b, err := sqlite.Open(filepath.Join(t.TempDir(), "memory.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, b.Close()) })
		return b
	})
}
