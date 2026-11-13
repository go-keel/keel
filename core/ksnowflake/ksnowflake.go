// Package ksnowflake generates 64-bit unique IDs (snowflake: 41-bit timestamp, 10-bit node ID, 12-bit sequence).
//
// One Generator per process, injected wherever IDs are needed: a node ID is unique only if a single snowflake node
// uses it, so two generators with the same node ID would issue the same ID in the same millisecond. The node ID is
// either fixed in config (NewStatic) or leased from Redis (NewAuto); di.go picks one explicitly according to
// app.node_id.
//
// A leased node ID is renewed every 10s with a 30s TTL. Once 20s pass without a successful renewal, Next returns
// ErrLeaseExpired instead of an ID: by then another instance may soon own the same node ID, and a duplicate ID written
// to a database cannot be taken back, while a failed request can be retried. If renewal finds the node ID taken by
// another instance, the generator claims a free one and continues; the process never exits on its own.
package ksnowflake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/snowflake"
	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/core/kconfig"
	"github.com/go-keel/keel/internal/appid"
	"github.com/go-keel/keel/internal/errs"
)

// The lease timings are fixed, not config: issuing must stop before the lease can expire (stopAfter < leaseTTL), and
// a configurable pair could be set the other way round.
const (
	maxNodeID       = 1023
	leaseTTL        = 30 * time.Second
	renewEvery      = 10 * time.Second
	stopAfter       = 20 * time.Second // two missed renewals; 10s margin for network delay and clock drift
	allocateTimeout = 15 * time.Second
	releaseTimeout  = 3 * time.Second
)

var (
	// ErrLeaseExpired is returned by Next when the leased node ID could not be renewed for 20s, or was taken by
	// another instance and no free node ID could be claimed yet. Issuing resumes on its own once the lease is back.
	ErrLeaseExpired = errors.New("ksnowflake: node ID lease not confirmed; no IDs are issued until it is renewed")
	// ErrNoAvailableNodeID is returned by NewAuto when all node IDs (0-1023) are leased.
	ErrNoAvailableNodeID = errors.New("ksnowflake: all node IDs (0-1023) are leased")

	errLeaseLost = errors.New("node ID lease belongs to another instance")
)

// ID is a snowflake ID. The bit widths and the epoch (2010-11-04) are bwmarrin/snowflake's defaults; the epoch only
// has to be the same across services, so never change it in one service.
type ID snowflake.ID

// Int64 returns the ID as int64.
func (id ID) Int64() int64 { return int64(id) }

// Uint64 returns the ID as uint64, the form stored in a BIGINT UNSIGNED column.
func (id ID) Uint64() uint64 { return uint64(id) }

// String returns the decimal form.
func (id ID) String() string { return snowflake.ID(id).String() }

// Base58 returns a short form for URLs.
func (id ID) Base58() string { return snowflake.ID(id).Base58() }

// Generator issues unique IDs. Create one per process with NewStatic or NewAuto and share it.
type Generator struct {
	mu        sync.Mutex
	node      *snowflake.Node
	nodeID    int64
	leased    bool      // false: fixed node ID, Next never fails
	lastRenew time.Time // last confirmed lease (monotonic reading)
	lost      bool      // the node ID belongs to another instance; issuing stops until a new one is claimed

	now        func() time.Time
	rdb        redis.UniversalClient
	keyPrefix  string
	instanceID string
	cancel     context.CancelFunc
	done       chan struct{}
}

// NewStatic uses the node ID fixed in app.node_id ([0, 1023]) and never touches Redis.
//
// NewStatic and NewAuto are two constructors rather than a branch on the config value, so whether assembly needs a
// Redis connection is visible in di.go; a service with a fixed node ID does not need Redis for IDs.
func NewStatic(config *kconfig.Config) (*Generator, error) {
	nodeID := config.App.GetNodeId()
	if nodeID < 0 {
		return nil, errs.Rules.New("ksnowflake.allocator-mismatch",
			fmt.Sprintf("di.go uses ksnowflake.NewStatic but app.node_id is %d", nodeID),
			"use ksnowflake.NewAuto in di.go, or set app.node_id to a fixed ID in [0, 1023]")
	}
	node, err := snowflake.NewNode(nodeID)
	if err != nil {
		return nil, err
	}
	slog.Info("snowflake IDs use a fixed node ID", "node_id", nodeID)
	return &Generator{node: node, nodeID: nodeID, now: time.Now}, nil
}

// NewAuto leases a free node ID from Redis when app.node_id is -1 and renews it in the background. The cleanup stops
// renewal and releases the node ID; the Redis connection belongs to its own provider.
func NewAuto(config *kconfig.Config, rdb redis.UniversalClient) (*Generator, func(), error) {
	nodeID := config.App.GetNodeId()
	if nodeID >= 0 {
		return nil, nil, errs.Rules.New("ksnowflake.allocator-mismatch",
			fmt.Sprintf("di.go uses ksnowflake.NewAuto but app.node_id is %d", nodeID),
			"use ksnowflake.NewStatic in di.go, or set app.node_id to -1 for automatic allocation")
	}
	domain := config.App.GetDomain()
	if domain == "" {
		domain = "keel"
	}
	g := &Generator{
		leased: true,
		now:    time.Now,
		rdb:    rdb,
		// The application instance ID plus a per-process token (appid.LeaseOwner): the logs still match the instance,
		// and two processes on one host never mistake each other's lease for their own.
		instanceID: appid.LeaseOwner(config.App),
		keyPrefix:  fmt.Sprintf("%s:%s:node_id:", domain, config.App.GetName()),
	}
	ctx, cancel := context.WithTimeout(context.Background(), allocateTimeout)
	defer cancel()
	if err := g.claim(ctx); err != nil {
		return nil, nil, err
	}
	runCtx, stop := context.WithCancel(context.Background())
	g.cancel, g.done = stop, make(chan struct{})
	go g.renewLoop(runCtx)
	return g, g.release, nil
}

// Next returns a new unique ID, or ErrLeaseExpired while a leased node ID is not confirmed.
func (g *Generator) Next() (ID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.leased && (g.lost || g.now().Sub(g.lastRenew) >= stopAfter) {
		return 0, ErrLeaseExpired
	}
	return ID(g.node.Generate()), nil
}

// NodeID returns the node ID in use (it changes if a leased node ID is lost and a new one is claimed).
func (g *Generator) NodeID() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.nodeID
}

func (g *Generator) key(id int64) string { return fmt.Sprintf("%s%d", g.keyPrefix, id) }

// claim leases a free node ID: one pipelined EXISTS over all 1024 keys, then SET NX on the free ones (a race between
// the two is covered by NX).
func (g *Generator) claim(ctx context.Context) error {
	cmds := make([]*redis.IntCmd, maxNodeID+1)
	pipe := g.rdb.Pipeline()
	for id := range int64(maxNodeID + 1) {
		cmds[id] = pipe.Exists(ctx, g.key(id))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("ksnowflake: scan node ID leases: %w", err)
	}
	for id := range int64(maxNodeID + 1) {
		if cmds[id].Val() != 0 {
			continue
		}
		ok, err := g.rdb.SetNX(ctx, g.key(id), g.instanceID, leaseTTL).Result()
		if err != nil {
			return fmt.Errorf("ksnowflake: lease node ID %d: %w", id, err)
		}
		if !ok {
			continue
		}
		node, err := snowflake.NewNode(id)
		if err != nil {
			return err
		}
		g.mu.Lock()
		g.node, g.nodeID, g.lastRenew, g.lost = node, id, g.now(), false
		g.mu.Unlock()
		slog.Info("snowflake IDs use a leased node ID", "node_id", id, "instance_id", g.instanceID)
		return nil
	}
	return ErrNoAvailableNodeID
}

func (g *Generator) renewLoop(ctx context.Context) {
	defer close(g.done)
	t := time.NewTicker(renewEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.renew(ctx)
		}
	}
}

var renewScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("expire", KEYS[1], ARGV[2])
end
return 0
`)

// renew extends the lease. A transient failure only logs (Next stops issuing by itself after stopAfter); a lease held
// by another instance stops issuing at once and claims a new node ID.
func (g *Generator) renew(ctx context.Context) {
	g.mu.Lock()
	id, lost := g.nodeID, g.lost
	g.mu.Unlock()

	if !lost {
		res, err := renewScript.Run(ctx, g.rdb, []string{g.key(id)}, g.instanceID, int(leaseTTL.Seconds())).Int64()
		switch {
		case err != nil:
			slog.Error("snowflake node ID renewal failed; IDs stop after 20s without renewal", "node_id", id, "error", err)
			return
		case res == 1:
			g.mu.Lock()
			g.lastRenew = g.now()
			g.mu.Unlock()
			return
		}
		g.mu.Lock()
		g.lost = true
		g.mu.Unlock()
		slog.Warn("snowflake node ID lease taken by another instance; claiming a new one", "node_id", id, "error", errLeaseLost)
	}
	if err := g.claim(ctx); err != nil {
		slog.Error("snowflake node ID could not be claimed; no IDs are issued until one is", "error", err)
	}
}

var releaseScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0
`)

// release stops renewal and deletes the lease if this instance still owns it.
func (g *Generator) release() {
	g.cancel()
	<-g.done
	g.mu.Lock()
	id := g.nodeID
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	if _, err := releaseScript.Run(ctx, g.rdb, []string{g.key(id)}, g.instanceID).Int64(); err != nil {
		slog.Error("snowflake node ID release failed; it expires within 30s", "node_id", id, "error", err)
	}
}
