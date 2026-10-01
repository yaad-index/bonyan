package inmem_test

import (
	"testing"

	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/memory/inmem"
	"github.com/yaad-index/bonyan/memory/memorytest"
)

func TestConformance(t *testing.T) {
	memorytest.Run(t, func(*testing.T) memory.Backend { return inmem.New() })
}
