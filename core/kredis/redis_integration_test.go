//go:build integration

package kredis_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
	"github.com/go-keel/keel/core/kredis"
	"github.com/go-keel/keel/internal/integrationtest"
)

// A client built from a named redis section connects to the configured server, reads and writes there, and is
// closed by the cleanup.
func TestNewNamedRedis_ConnectsAndCleanupCloses(t *testing.T) {
	cfg := &kconfig.Config{Config: &conf.Config{
		App:   &conf.App{Name: "it"},
		Redis: map[string]*conf.Redis{"cache": {Addr: []string{integrationtest.Env(t, "TEST_REDIS_ADDR")}}},
	}}
	r, cleanup, err := kredis.NewNamedRedis(cfg, "cache")
	if err != nil {
		t.Fatalf("NewNamedRedis: %v", err)
	}
	ctx := context.Background()
	key := fmt.Sprintf("it:kredis:%s:%d", t.Name(), time.Now().UnixNano())

	if err := r.Set(ctx, key, "v", time.Minute).Err(); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, err := r.Get(ctx, key).Result(); err != nil || got != "v" {
		t.Fatalf("get = %q, %v; want v", got, err)
	}
	r.Del(ctx, key)

	cleanup()
	if err := r.Ping(ctx).Err(); !errors.Is(err, redis.ErrClosed) {
		t.Fatalf("ping after cleanup = %v, want redis.ErrClosed", err)
	}
}
