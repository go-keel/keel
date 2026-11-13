//go:build integration

package kredis_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
	"github.com/go-keel/keel/core/kredis"
	"github.com/go-keel/keel/core/kredis/lock"
	"github.com/go-keel/keel/core/kredis/ratelimit"
	"github.com/go-keel/keel/internal/integrationtest"
)

func newRedis(t *testing.T) *kredis.Redis {
	t.Helper()
	cfg := &kconfig.Config{Config: &conf.Config{
		App:   &conf.App{Name: "it"},
		Redis: map[string]*conf.Redis{"default": {Addr: []string{integrationtest.Env(t, "TEST_REDIS_ADDR")}}},
	}}
	r, cleanup, err := kredis.NewRedis(cfg)
	if err != nil {
		t.Fatalf("NewRedis: %v", err)
	}
	t.Cleanup(cleanup)
	return r
}

// The lock's Lua scripts run on a real server: a held lock blocks a second holder until released.
func TestLock(t *testing.T) {
	r := newRedis(t)
	ctx := context.Background()
	l := lock.NewLocker(r)
	key := "it:lock:" + time.Now().Format(time.RFC3339Nano)

	release, err := l.TryAcquireLock(ctx, key, 10)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := l.TryAcquireLock(ctx, key, 10); !errors.Is(err, lock.ErrLockNotAcquired) {
		t.Fatalf("second acquire while held: got %v, want ErrLockNotAcquired", err)
	}
	release()
	again, err := l.TryAcquireLock(ctx, key, 10)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	again()
}

// The limiter's script counts on a real server: the call over the limit inside the window is rejected.
func TestRateLimit(t *testing.T) {
	r := newRedis(t)
	ctx := context.Background()
	lim, err := ratelimit.New(r, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	key := "it:rl:" + time.Now().Format(time.RFC3339Nano)
	for i := range 2 {
		if err := lim.Allow(ctx, key, 2, time.Minute); err != nil {
			t.Fatalf("call %d within limit: %v", i+1, err)
		}
	}
	if err := lim.Allow(ctx, key, 2, time.Minute); err == nil {
		t.Fatal("third call over the limit was allowed")
	}
}
