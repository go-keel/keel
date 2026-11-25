//go:build integration

// In-package: checks the scheduler lease and replicas through the lease owner values.

package kasynq

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
	"github.com/go-keel/keel/internal/integrationtest"
	"github.com/go-keel/keel/pkg/rule"
)

func leaseRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: integrationtest.Env(t, "TEST_REDIS_ADDR")})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// startReplica starts a SchedulerServer of application name with instance ID id on rdb; done receives Start's result.
func startReplica(t *testing.T, rdb redis.UniversalClient, name, id string) (s *SchedulerServer, done chan error) {
	t.Helper()
	cfg := &kconfig.Config{Config: &conf.Config{App: &conf.App{Name: name, Id: id}}}
	s, err := NewSchedulerServer(cfg, rdb, func(r ScheduleRegistrar) { _, _ = r.Register("@every 1h", "it:cron", nil) })
	if err != nil {
		t.Fatal(err)
	}
	done = make(chan error, 1)
	go func() { done <- s.Start(context.Background()) }()
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	return s, done
}

// leaseName returns a fresh application name and its lease key, deleted after the test.
func leaseName(t *testing.T, rdb redis.UniversalClient) (name, key string) {
	name = fmt.Sprintf("it-lease-%d", time.Now().UnixNano())
	key = "keel:" + name + ":cron_scheduler"
	t.Cleanup(func() { rdb.Del(context.Background(), key) })
	return name, key
}

// waitOwner polls the lease key until it holds want (a full owner value, or "" for no holder).
func waitOwner(t *testing.T, rdb redis.UniversalClient, key, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got, _ := rdb.Get(context.Background(), key).Result()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lease %s = %q, want %q within %v", key, got, want, within)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitHeldBy waits until one of replicas holds the lease and returns its index.
func waitHeldBy(t *testing.T, rdb redis.UniversalClient, key string, replicas ...*SchedulerServer) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := rdb.Get(context.Background(), key).Result()
		for i, s := range replicas {
			if got == s.lease.owner {
				return i
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("lease %s = %q, want one of the replicas to hold it within 5s", key, got)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func wantLeaseLost(t *testing.T, done chan error, within time.Duration) {
	t.Helper()
	select {
	case err := <-done:
		if re, ok := errors.AsType[*rule.Error](err); !ok || re.ID != "kasynq.scheduler-lease-lost" {
			t.Fatalf("Start returned %v, want kasynq.scheduler-lease-lost", err)
		}
	case <-time.After(within):
		t.Fatalf("Start did not return kasynq.scheduler-lease-lost within %v", within)
	}
}

// A holder that crashes neither renews nor releases: once its lease expires a standby takes over, and the cut-off holder
// itself stops scheduling 20s after its last renewal. Closing the holder's own Redis client stands in for the crash;
// shortening the key's TTL stands in for the 30s it would take to expire.
func TestScheduler_StandbyTakesOverAfterHolderCrash(t *testing.T) {
	rdb := leaseRedis(t)
	name, key := leaseName(t, rdb)
	aRDB := redis.NewClient(&redis.Options{Addr: rdb.Options().Addr})
	a, aDone := startReplica(t, aRDB, name, "a")
	waitOwner(t, rdb, key, a.lease.owner, 5*time.Second)
	b, _ := startReplica(t, rdb, name, "b")

	_ = aRDB.Close()
	if err := rdb.PExpire(context.Background(), key, time.Millisecond).Err(); err != nil {
		t.Fatal(err)
	}
	waitOwner(t, rdb, key, b.lease.owner, 15*time.Second) // a standby tries every 10s
	wantLeaseLost(t, aDone, 25*time.Second)               // 20s after a's last renewal
}

// The holder renews the lease every 10s while a standby waits, so the lease never expires under a live holder.
// Shortening the key's TTL makes the renewal visible: it puts the TTL back to 30s.
func TestScheduler_HolderRenewsWithStandby(t *testing.T) {
	rdb := leaseRedis(t)
	ctx := context.Background()
	name, key := leaseName(t, rdb)
	a, _ := startReplica(t, rdb, name, "a")
	waitOwner(t, rdb, key, a.lease.owner, 5*time.Second)
	startReplica(t, rdb, name, "b")

	if err := rdb.PExpire(ctx, key, 15*time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		ttl, err := rdb.PTTL(ctx, key).Result()
		if err != nil {
			t.Fatal(err)
		}
		if ttl > 20*time.Second {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lease TTL = %v, want it renewed to 30s within 15s", ttl)
		}
		time.Sleep(100 * time.Millisecond)
	}
	waitOwner(t, rdb, key, a.lease.owner, 0)
}

// When the lease expires under a holder (a stall longer than the TTL) and a standby takes it, the old holder's next
// renewal finds another owner: it stops scheduling and Start returns kasynq.scheduler-lease-lost, leaving the new
// holder's lease alone. Deleting the key stands in for the expiry.
func TestScheduler_HolderStopsWhenLeaseTaken(t *testing.T) {
	rdb := leaseRedis(t)
	ctx := context.Background()
	name, key := leaseName(t, rdb)
	a, aDone := startReplica(t, rdb, name, "a")
	waitOwner(t, rdb, key, a.lease.owner, 5*time.Second)
	b, _ := startReplica(t, rdb, name, "b")

	if err := rdb.Del(ctx, key).Err(); err != nil {
		t.Fatal(err)
	}
	wantLeaseLost(t, aDone, 15*time.Second)
	waitOwner(t, rdb, key, b.lease.owner, 15*time.Second)
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("a Stop: %v", err)
	}
	waitOwner(t, rdb, key, b.lease.owner, 0)
}

// Two replicas with the same instance ID (two processes on one machine) are still two lease owners: one holds the
// lease, the other waits, and stopping the waiting one does not release the holder's lease.
func TestScheduler_SameInstanceIDReplicas(t *testing.T) {
	rdb := leaseRedis(t)
	ctx := context.Background()
	name, key := leaseName(t, rdb)
	r0, d0 := startReplica(t, rdb, name, "same")
	r1, d1 := startReplica(t, rdb, name, "same")
	replicas, dones := []*SchedulerServer{r0, r1}, []chan error{d0, d1}
	h := waitHeldBy(t, rdb, key, r0, r1)
	holder, other := replicas[h], replicas[1-h]

	// The other must not take or share a held lease: check throughout half a second of its attempts.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		waitOwner(t, rdb, key, holder.lease.owner, 0)
		time.Sleep(100 * time.Millisecond)
	}
	if err := other.Stop(ctx); err != nil {
		t.Fatalf("waiting replica Stop: %v", err)
	}
	if err := <-dones[1-h]; err != nil {
		t.Fatalf("waiting replica Start returned %v after Stop, want nil", err)
	}
	waitOwner(t, rdb, key, holder.lease.owner, 0)
}
