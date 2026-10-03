package runstore_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/runstore"
	"github.com/yaad-index/bonyan/runstore/runstoretest"
)

func TestMemory(t *testing.T) {
	runstoretest.Run(t, func(*testing.T) runstore.Store { return runstore.NewMemory() })
}

func TestDir(t *testing.T) {
	runstoretest.Run(t, func(t *testing.T) runstore.Store {
		d, err := runstore.OpenDir(t.TempDir() + "/runs")
		require.NoError(t, err)
		return d
	})
}

// TestHelperClaim is run as its own process by TestDirAcrossProcesses: it
// claims the run and prints whether it won.
func TestHelperClaim(t *testing.T) {
	dir := os.Getenv("RUNSTORE_HELPER_DIR")
	if dir == "" {
		t.Skip("run by TestDirAcrossProcesses")
	}
	d, err := runstore.OpenDir(dir)
	require.NoError(t, err)
	_, err = d.Claim(context.Background(), "r1", os.Getenv("RUNSTORE_HELPER_TOKEN"), time.Now())
	switch {
	case err == nil:
		fmt.Println("RESULT won")
	case errors.Is(err, runstore.ErrClaimed):
		fmt.Println("RESULT claimed")
	default:
		require.NoError(t, err)
	}
}

// Processes sharing a Dir claim a run once between them.
func TestDirAcrossProcesses(t *testing.T) {
	dir := t.TempDir() + "/runs"
	d, err := runstore.OpenDir(dir)
	require.NoError(t, err)
	require.NoError(t, d.Save(context.Background(), runstore.State{Run: "r1", Subject: "ana", Saved: time.Now(), Deadline: time.Now().Add(time.Hour)}, ""))
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := map[string]int{}
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperClaim$", "-test.count=1")
			cmd.Env = append(os.Environ(), "RUNSTORE_HELPER_DIR="+dir, fmt.Sprintf("RUNSTORE_HELPER_TOKEN=t%d", i))
			out, err := cmd.CombinedOutput()
			if !assert.NoError(t, err, "%s", out) {
				return
			}
			for _, line := range strings.Split(string(out), "\n") {
				if r, ok := strings.CutPrefix(line, "RESULT "); ok {
					mu.Lock()
					results[r]++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, map[string]int{"won": 1, "claimed": 7}, results)
}
