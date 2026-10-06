package record

import (
	"bytes"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A recording written before a request field was added must still replay, so a
// field the request leaves unset takes no part in the fingerprint.
func TestARecordedFingerprintIsReproducedFromItsRequest(t *testing.T) {
	raw, err := os.ReadFile("testdata/v1.jsonl")
	require.NoError(t, err)
	_, calls, err := Read(bytes.NewReader(raw))
	require.NoError(t, err)
	require.NotEmpty(t, calls)
	for _, c := range calls {
		require.NotNil(t, c.Request, "call %d", c.Seq)
		assert.Equal(t, c.Fingerprint, fingerprint(c.Kind, c.Model, *c.Request), "call %d", c.Seq)
	}
}
