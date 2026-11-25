package ksnowflake

import (
	"sync"
	"testing"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
)

func cfg(nodeID int64) *kconfig.Config {
	return &kconfig.Config{Config: &conf.Config{App: &conf.App{Name: "svc", Domain: "keel", Id: "i1", NodeId: nodeID}}}
}

func TestStaticGeneratorIssuesWithFixedNodeID(t *testing.T) {
	g, err := NewStatic(cfg(42))
	if err != nil {
		t.Fatal(err)
	}
	if g.NodeID() != 42 {
		t.Errorf("NodeID = %d, want 42", g.NodeID())
	}
	if _, err := g.Next(); err != nil {
		t.Errorf("Next: %v", err)
	}
}

func TestConstructorMustMatchNodeIDConfig(t *testing.T) {
	if _, err := NewStatic(cfg(-1)); err == nil {
		t.Error("NewStatic with app.node_id -1: want an error")
	}
	if _, _, err := NewAuto(cfg(5), nil); err == nil {
		t.Error("NewAuto with a fixed app.node_id: want an error")
	}
}

// Every user of the process-wide generator gets distinct IDs, also within one millisecond from many goroutines.
func TestSharedGeneratorNeverRepeatsAnID(t *testing.T) {
	g, err := NewStatic(cfg(1))
	if err != nil {
		t.Fatal(err)
	}
	const workers, each = 8, 20000
	ids := make(chan ID, workers*each)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range each {
				id, err := g.Next()
				if err != nil {
					t.Error(err)
					return
				}
				ids <- id
			}
		})
	}
	wg.Wait()
	close(ids)
	seen := make(map[ID]bool, workers*each)
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate ID %d", id)
		}
		seen[id] = true
	}
}
