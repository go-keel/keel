//go:build integration

package redislease_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/internal/integrationtest"
	"github.com/go-keel/keel/internal/redislease"
)

// Only one owner holds the lease; renewal and release fail for the others without touching it, and a release by the
// holder lets another owner take it at once.
func TestLease_OneHolder(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: integrationtest.Env(t, "TEST_REDIS_ADDR")})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	key := fmt.Sprintf("it:%s:%d", t.Name(), time.Now().UnixNano())
	t.Cleanup(func() { rdb.Del(context.Background(), key) })

	if err := redislease.Acquire(ctx, rdb, key, "a"); err != nil {
		t.Fatalf("a acquire = %v, want nil", err)
	}
	if ttl := rdb.TTL(ctx, key).Val(); ttl != redislease.TTL {
		t.Errorf("lease TTL = %v, want %v", ttl, redislease.TTL)
	}
	if err := redislease.Acquire(ctx, rdb, key, "b"); !errors.Is(err, redislease.ErrNotHeld) {
		t.Fatalf("b acquire = %v, want ErrNotHeld while a holds it", err)
	}
	if err := redislease.Renew(ctx, rdb, key, "a"); err != nil {
		t.Fatalf("a renew = %v, want nil", err)
	}
	if err := redislease.Renew(ctx, rdb, key, "b"); !errors.Is(err, redislease.ErrNotHeld) {
		t.Fatalf("b renew = %v, want ErrNotHeld", err)
	}
	if err := redislease.Release(rdb, key, "b"); !errors.Is(err, redislease.ErrNotHeld) {
		t.Fatalf("b release = %v, want ErrNotHeld", err)
	}
	if got, _ := rdb.Get(ctx, key).Result(); got != "a" {
		t.Fatalf("release by a non-holder deleted the lease (holder now %q)", got)
	}
	if err := redislease.Release(rdb, key, "a"); err != nil {
		t.Fatalf("a release = %v, want nil", err)
	}
	if err := redislease.Acquire(ctx, rdb, key, "b"); err != nil {
		t.Fatalf("b acquire after release = %v, want nil", err)
	}
}
