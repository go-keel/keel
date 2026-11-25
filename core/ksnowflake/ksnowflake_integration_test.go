//go:build integration

// In-package: drives the node-ID lease with an injected clock, not reachable through the public API.

package ksnowflake

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
	"github.com/go-keel/keel/internal/integrationtest"
	"github.com/go-keel/keel/internal/redislease"
)

// leaseCfg gives each test its own app name, so its node-ID keys (keel:<name>:node_id:<n>) do not meet another
// test's or another run's on the shared server.
func leaseCfg(t *testing.T) *kconfig.Config {
	name := fmt.Sprintf("it-%s-%d", t.Name(), time.Now().UnixNano())
	return &kconfig.Config{Config: &conf.Config{App: &conf.App{Name: name, Domain: "keel", Id: "i1", NodeId: -1}}}
}

func leaseKey(c *kconfig.Config, id int64) string {
	return fmt.Sprintf("keel:%s:node_id:%d", c.App.GetName(), id)
}

// faultHook makes every command fail without reaching the server while it is on.
type faultHook struct{ on atomic.Bool }

var errInjected = errors.New("injected redis failure")

func (h *faultHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *faultHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if h.on.Load() {
			cmd.SetErr(errInjected)
			return errInjected
		}
		return next(ctx, cmd)
	}
}

func (h *faultHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if h.on.Load() {
			for _, c := range cmds {
				c.SetErr(errInjected)
			}
			return errInjected
		}
		return next(ctx, cmds)
	}
}

func newClient(t *testing.T) (*redis.Client, *faultHook) {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: integrationtest.Env(t, "TEST_REDIS_ADDR")})
	t.Cleanup(func() { _ = rdb.Close() })
	h := &faultHook{}
	rdb.AddHook(h)
	return rdb, h
}

// occupyAll leases every node ID to another owner, and deletes those leases when the test ends.
func occupyAll(t *testing.T, rdb *redis.Client, c *kconfig.Config) {
	t.Helper()
	ctx := context.Background()
	keys := make([]string, 0, maxNodeID+1)
	pipe := rdb.Pipeline()
	for id := range int64(maxNodeID + 1) {
		k := leaseKey(c, id)
		keys = append(keys, k)
		pipe.Set(ctx, k, "other", time.Minute)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rdb.Del(context.Background(), keys...) })
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// newAuto leases from the real server and puts the generator on a manual clock; renewals are driven by calling
// renew, the background loop only ticks every 10s of real time.
func newAuto(t *testing.T) (*Generator, *kconfig.Config, *redis.Client, *faultHook, *clock) {
	t.Helper()
	rdb, h := newClient(t)
	c := leaseCfg(t)
	g, cleanup, err := NewAuto(c, rdb)
	if err != nil {
		t.Fatalf("NewAuto: %v", err)
	}
	t.Cleanup(cleanup)
	clk := &clock{t: time.Unix(1_000_000, 0)}
	g.mu.Lock()
	g.now, g.lastRenew = clk.now, clk.now()
	g.mu.Unlock()
	return g, c, rdb, h, clk
}

func TestAutoLeasesAndReleasesANodeID(t *testing.T) {
	rdb, _ := newClient(t)
	ctx := context.Background()
	c := leaseCfg(t)
	g, cleanup, err := NewAuto(c, rdb)
	if err != nil {
		t.Fatal(err)
	}
	key := leaseKey(c, g.NodeID())
	if owner, _ := rdb.Get(ctx, key).Result(); !strings.HasPrefix(owner, "i1/") {
		t.Fatalf("lease %s owned by %q, want the instance ID i1 plus a token", key, owner)
	}
	cleanup()
	if n := rdb.Exists(ctx, key).Val(); n != 0 {
		t.Error("cleanup must delete the lease")
	}
}

func TestIssuingStopsWithoutRenewalAndResumes(t *testing.T) {
	g, _, _, fault, c := newAuto(t)
	ctx := context.Background()

	c.add(redislease.StopAfter - time.Second)
	if _, err := g.Next(); err != nil {
		t.Fatalf("19s after renewal: %v", err)
	}
	fault.on.Store(true)
	g.renew(ctx) // fails: no new confirmation
	c.add(time.Second)
	if _, err := g.Next(); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("20s without renewal: got %v, want ErrLeaseExpired", err)
	}
	fault.on.Store(false)
	g.renew(ctx)
	if _, err := g.Next(); err != nil {
		t.Fatalf("after renewal recovers: %v", err)
	}
}

func TestLeaseTakenByAnotherInstanceClaimsANewNodeID(t *testing.T) {
	g, c, rdb, _, _ := newAuto(t)
	old := g.NodeID()
	if err := rdb.Set(context.Background(), leaseKey(c, old), "other", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rdb.Del(context.Background(), leaseKey(c, old)) })
	g.renew(context.Background())
	if g.NodeID() == old {
		t.Fatalf("still on node ID %d after it was taken", old)
	}
	if _, err := g.Next(); err != nil {
		t.Fatalf("Next on the new node ID: %v", err)
	}
}

func TestNoFreeNodeID(t *testing.T) {
	rdb, _ := newClient(t)
	c := leaseCfg(t)
	occupyAll(t, rdb, c)
	if _, _, err := NewAuto(c, rdb); !errors.Is(err, ErrNoAvailableNodeID) {
		t.Fatalf("NewAuto with every node ID leased: got %v, want ErrNoAvailableNodeID", err)
	}
}

func TestLostLeaseWithNoFreeNodeIDStopsIssuing(t *testing.T) {
	g, c, rdb, _, _ := newAuto(t)
	occupyAll(t, rdb, c)
	g.renew(context.Background())
	if _, err := g.Next(); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("lost lease, nothing free: got %v, want ErrLeaseExpired", err)
	}
}
