package runstore_test

import (
	"testing"

	"github.com/yaad-index/bonyan/runstore"
	"github.com/yaad-index/bonyan/runstore/runstoretest"
)

func TestMemory(t *testing.T) {
	runstoretest.Run(t, func(*testing.T) runstore.Store { return runstore.NewMemory() })
}
