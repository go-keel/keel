// In-package: checks the scheduler lease directly; its helpers are shared with scheduler_internal_test.go.

package kasynq

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
)

func schedulerTestConfig() *kconfig.Config {
	return &kconfig.Config{Config: &conf.Config{App: &conf.App{Name: "svc", Id: "a"}}}
}

// Only one instance holds the lease; renewal fails for the others, release by a non-holder is a no-op, and a release
// by the holder lets another instance take it at once.
func TestLease_OneHolder(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	a := lease{rdb: rdb, key: "keel:svc:cron_scheduler", owner: "a"}
	b := lease{rdb: rdb, key: a.key, owner: "b"}

	if ok, err := a.acquire(ctx); !ok || err != nil {
		t.Fatalf("a acquire = %v, %v; want true", ok, err)
	}
	if ok, err := b.acquire(ctx); ok || err != nil {
		t.Fatalf("b acquire = %v, %v; want false while a holds it", ok, err)
	}
	if ok, err := a.renew(ctx); !ok || err != nil {
		t.Fatalf("a renew = %v, %v; want true", ok, err)
	}
	if ok, err := b.renew(ctx); ok || err != nil {
		t.Fatalf("b renew = %v, %v; want false, it does not hold the lease", ok, err)
	}
	b.release()
	if got, _ := mr.Get(a.key); got != "a" {
		t.Fatalf("release by a non-holder deleted the lease (holder now %q)", got)
	}
	a.release()
	if ok, err := b.acquire(ctx); !ok || err != nil {
		t.Fatalf("b acquire after release = %v, %v; want true", ok, err)
	}
}

// The lease key carries the application's domain and name, so applications sharing one Redis do not block each other.
func TestNewSchedulerServer_LeaseKey(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	cfg := schedulerTestConfig()
	cfg.App.Domain = "shop"
	s, err := NewSchedulerServer(cfg, rdb, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.lease.key != "shop:svc:cron_scheduler" || !strings.HasPrefix(s.lease.owner, "a/") {
		t.Fatalf("lease = %q owned by %q, want shop:svc:cron_scheduler owned by instance a plus a token", s.lease.key, s.lease.owner)
	}
}
