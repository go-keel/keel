// In-package: drives the node-ID lease with an injected clock and short TTL, not reachable through the public API.

package ksnowflake

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
)

func cfg(nodeID int64) *kconfig.Config {
	return &kconfig.Config{Config: &conf.Config{App: &conf.App{Name: "svc", Domain: "keel", Id: "i1", NodeId: nodeID}}}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// newAuto leases from a fresh miniredis and puts the generator on a manual clock; renewals are driven by calling
// renew, the background loop only ticks every 10s of real time.
func newAuto(t *testing.T) (*Generator, *miniredis.Miniredis, *clock) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	g, cleanup, err := NewAuto(cfg(-1), rdb)
	if err != nil {
		t.Fatalf("NewAuto: %v", err)
	}
	t.Cleanup(cleanup)
	c := &clock{t: time.Unix(1_000_000, 0)}
	g.mu.Lock()
	g.now, g.lastRenew = c.now, c.now()
	g.mu.Unlock()
	return g, mr, c
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

func TestAutoLeasesAndReleasesANodeID(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	g, cleanup, err := NewAuto(cfg(-1), rdb)
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("keel:svc:node_id:%d", g.NodeID())
	if owner, _ := mr.Get(key); !strings.HasPrefix(owner, "i1/") {
		t.Fatalf("lease %s owned by %q, want the instance ID i1 plus a token", key, owner)
	}
	if ttl := mr.TTL(key); ttl != leaseTTL {
		t.Errorf("lease TTL = %v, want %v", ttl, leaseTTL)
	}
	cleanup()
	if mr.Exists(key) {
		t.Error("cleanup must delete the lease")
	}
}

func TestIssuingStopsWithoutRenewalAndResumes(t *testing.T) {
	g, mr, c := newAuto(t)
	ctx := context.Background()

	c.add(stopAfter - time.Second)
	if _, err := g.Next(); err != nil {
		t.Fatalf("19s after renewal: %v", err)
	}
	mr.SetError("LOADING")
	g.renew(ctx) // fails: no new confirmation
	c.add(time.Second)
	if _, err := g.Next(); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("20s without renewal: got %v, want ErrLeaseExpired", err)
	}
	mr.SetError("")
	g.renew(ctx)
	if _, err := g.Next(); err != nil {
		t.Fatalf("after renewal recovers: %v", err)
	}
}

func TestLeaseTakenByAnotherInstanceClaimsANewNodeID(t *testing.T) {
	g, mr, _ := newAuto(t)
	old := g.NodeID()
	if err := mr.Set(fmt.Sprintf("keel:svc:node_id:%d", old), "other"); err != nil {
		t.Fatal(err)
	}
	g.renew(context.Background())
	if g.NodeID() == old {
		t.Fatalf("still on node ID %d after it was taken", old)
	}
	if _, err := g.Next(); err != nil {
		t.Fatalf("Next on the new node ID: %v", err)
	}
}

func TestNoFreeNodeID(t *testing.T) {
	mr := miniredis.RunT(t)
	for id := range maxNodeID + 1 {
		_ = mr.Set(fmt.Sprintf("keel:svc:node_id:%d", id), "other")
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	if _, _, err := NewAuto(cfg(-1), rdb); !errors.Is(err, ErrNoAvailableNodeID) {
		t.Fatalf("NewAuto with every node ID leased: got %v, want ErrNoAvailableNodeID", err)
	}
}

func TestLostLeaseWithNoFreeNodeIDStopsIssuing(t *testing.T) {
	g, mr, _ := newAuto(t)
	for id := range maxNodeID + 1 {
		_ = mr.Set(fmt.Sprintf("keel:svc:node_id:%d", id), "other")
	}
	g.renew(context.Background())
	if _, err := g.Next(); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("lost lease, nothing free: got %v, want ErrLeaseExpired", err)
	}
}
