//go:build integration

package kasynq_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kasynq"
	"github.com/go-keel/keel/core/kconfig"
	"github.com/go-keel/keel/internal/integrationtest"
)

func realRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: integrationtest.Env(t, "TEST_REDIS_ADDR")})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func asynqConfig() *kconfig.Config {
	return &kconfig.Config{Config: &conf.Config{
		App:   &conf.App{Name: "it"},
		Asynq: map[string]*conf.Asynq{"default": {}},
	}}
}

type ping struct{ N int }

// A task enqueued through the contract is run by a worker on a real Redis.
func TestEnqueueAndRun(t *testing.T) {
	rdb := realRedis(t)
	task := kasynq.NewTaskType[ping]("it:ping:" + time.Now().Format("150405.000000"))
	got := make(chan int, 1)
	w, err := kasynq.NewWorkerServer(asynqConfig(), rdb, func(r kasynq.HandlerRegistrar) {
		task.Handle(r, func(ctx context.Context, p ping) error { got <- p.N; return nil })
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = w.Stop(context.Background()) })

	if _, err := task.Enqueue(context.Background(), kasynq.NewTaskClient(rdb), ping{N: 7}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	select {
	case n := <-got:
		if n != 7 {
			t.Errorf("payload N = %d, want 7", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("task was not run within 10s")
	}
}

// A Redis that may evict any key is refused at start; volatile-* is accepted.
func TestStartRefusesAllKeysEviction(t *testing.T) {
	rdb := realRedis(t)
	ctx := context.Background()
	old, err := rdb.ConfigGet(ctx, "maxmemory-policy").Result()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rdb.ConfigSet(ctx, "maxmemory-policy", old["maxmemory-policy"]) })

	start := func() error {
		w, err := kasynq.NewWorkerServer(asynqConfig(), rdb, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = w.Start(ctx)
		if err == nil {
			_ = w.Stop(ctx)
		}
		return err
	}
	if err := rdb.ConfigSet(ctx, "maxmemory-policy", "allkeys-lru").Err(); err != nil {
		t.Fatal(err)
	}
	if err := start(); err == nil || !strings.Contains(err.Error(), "allkeys-lru") {
		t.Errorf("allkeys-lru: got %v, want a refusal naming the policy", err)
	}
	if err := rdb.ConfigSet(ctx, "maxmemory-policy", "volatile-lru").Err(); err != nil {
		t.Fatal(err)
	}
	if err := start(); err != nil {
		t.Errorf("volatile-lru: got %v, want start", err)
	}
}

// Two scheduler replicas: one holds the lease and schedules, the other waits; a clean Stop hands the lease over, and
// Start returns nil on Stop in both states.
func TestSchedulerReplicasShareOneLease(t *testing.T) {
	rdb := realRedis(t)
	ctx := context.Background()
	name := "it-cron-" + time.Now().Format("150405.000000")
	key := "keel:" + name + ":cron_scheduler"
	t.Cleanup(func() { rdb.Del(ctx, key) })

	newReplica := func(id string) (*kasynq.SchedulerServer, chan error) {
		cfg := &kconfig.Config{Config: &conf.Config{App: &conf.App{Name: name, Id: id}}}
		s, err := kasynq.NewSchedulerServer(cfg, rdb, func(r kasynq.ScheduleRegistrar) {
			_, _ = r.Register("@every 1h", "it:cron", nil)
		})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- s.Start(ctx) }()
		return s, done
	}
	holder := func() string { v, _ := rdb.Get(ctx, key).Result(); return v }
	waitHolder := func(want string, within time.Duration) {
		t.Helper()
		deadline := time.Now().Add(within)
		for holder() != want {
			if time.Now().After(deadline) {
				t.Fatalf("lease holder = %q, want %q within %v", holder(), want, within)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	a, aDone := newReplica("a")
	waitHolder("a", 5*time.Second)
	b, bDone := newReplica("b")
	// b must not take a held lease: check the holder throughout half a second of b's attempts.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := holder(); got != "a" {
			t.Fatalf("lease holder = %q after b started, want a", got)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := a.Stop(ctx); err != nil {
		t.Fatalf("a Stop: %v", err)
	}
	if err := <-aDone; err != nil {
		t.Fatalf("a Start returned %v after Stop, want nil", err)
	}
	waitHolder("b", 15*time.Second) // a standby tries every 10s

	if err := b.Stop(ctx); err != nil {
		t.Fatalf("b Stop: %v", err)
	}
	if err := <-bDone; err != nil {
		t.Fatalf("b Start returned %v after Stop, want nil", err)
	}
	if got := holder(); got != "" {
		t.Fatalf("lease still held by %q after the holder stopped", got)
	}
}
