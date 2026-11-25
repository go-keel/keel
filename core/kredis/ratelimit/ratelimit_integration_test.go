//go:build integration

package ratelimit_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/core/kredis/ratelimit"
	"github.com/go-keel/keel/internal/integrationtest"
)

func newLimiter(t *testing.T, bucket time.Duration) (*ratelimit.Limiter, *redis.Client) {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: integrationtest.Env(t, "TEST_REDIS_ADDR")})
	t.Cleanup(func() { _ = rdb.Close() })
	l, err := ratelimit.New(rdb, bucket)
	if err != nil {
		t.Fatal(err)
	}
	return l, rdb
}

// testKey is unique per test and run, so runs against the same server do not share counters.
func testKey(t *testing.T, name string) string {
	return fmt.Sprintf("it:rl:%s:%s:%d", t.Name(), name, time.Now().UnixNano())
}

func TestAllow_UnderLimit(t *testing.T) {
	l, _ := newLimiter(t, time.Second)
	ctx := context.Background()
	k := testKey(t, "k")
	for i := 0; i < 3; i++ {
		if err := l.Allow(ctx, k, 3, time.Minute); err != nil {
			t.Fatalf("request %d should be allowed: %v", i+1, err)
		}
	}
}

func TestAllow_ExceedsLimit(t *testing.T) {
	l, _ := newLimiter(t, time.Second)
	ctx := context.Background()
	k := testKey(t, "k")
	for i := 0; i < 3; i++ {
		_ = l.Allow(ctx, k, 3, time.Minute)
	}
	err := l.Allow(ctx, k, 3, time.Minute)
	if !errors.Is(err, ratelimit.ErrLimitExceeded) {
		t.Fatalf("expected ErrLimitExceeded, got %v", err)
	}
}

func TestAllow_IndependentKeys(t *testing.T) {
	l, _ := newLimiter(t, time.Second)
	ctx := context.Background()
	a, b := testKey(t, "a"), testKey(t, "b")
	for i := 0; i < 3; i++ {
		_ = l.Allow(ctx, a, 3, time.Minute)
	}
	if err := l.Allow(ctx, b, 3, time.Minute); err != nil {
		t.Fatalf("key b should be independent: %v", err)
	}
}

// The window must slide because the script trims old buckets by bucket index, not because the whole key expires.
// The script reads the server's clock (redis TIME), so the test waits for it: with 100ms buckets and a 300ms window
// the key's TTL is at least one second (expiry is set in whole seconds), so after 350ms the hash is still there and
// an allow can only come from old buckets being trimmed.
func TestAllow_WindowSlides(t *testing.T) {
	l, rdb := newLimiter(t, 100*time.Millisecond)
	ctx := context.Background()
	k := testKey(t, "k")
	const window = 300 * time.Millisecond
	for i := 0; i < 3; i++ {
		if err := l.Allow(ctx, k, 3, window); err != nil {
			t.Fatalf("request %d should be allowed: %v", i+1, err)
		}
	}
	if err := l.Allow(ctx, k, 3, window); !errors.Is(err, ratelimit.ErrLimitExceeded) {
		t.Fatalf("in-window request should be limited, got %v", err)
	}
	time.Sleep(350 * time.Millisecond)
	if n, err := rdb.Exists(ctx, k).Result(); err != nil || n != 1 {
		t.Fatalf("key should still exist after 350ms (got %d, %v); otherwise the allow below proves nothing", n, err)
	}
	if err := l.Allow(ctx, k, 3, window); err != nil {
		t.Fatalf("after window slides should be allowed: %v", err)
	}
}

// Storage follows only the number of buckets and is independent of limit: multiplying limit by ten thousand leaves
// the field count unchanged. This is the whole reason the ZSET approach was replaced, so it needs a test.
func TestStorageIndependentOfLimit(t *testing.T) {
	l, rdb := newLimiter(t, 100*time.Millisecond)
	ctx := context.Background()
	k := testKey(t, "k")
	for i := 0; i < 50; i++ {
		if err := l.Allow(ctx, k, 100000, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	// window/bucket = 10 buckets; the 50 requests land in the first few buckets, so there are far fewer fields than
	// requests.
	fields, err := rdb.HKeys(ctx, k).Result()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(fields); got > 10 {
		t.Fatalf("hash has %d fields, want <= 10 (bucket count); storage must not scale with request count", got)
	}
}
