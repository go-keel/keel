package ratelimit_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/core/kredis/ratelimit"
)

func newLimiter(t *testing.T, bucket time.Duration) (*ratelimit.Limiter, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	l, err := ratelimit.New(rdb, bucket)
	if err != nil {
		t.Fatal(err)
	}
	return l, mr
}

func TestAllow_UnderLimit(t *testing.T) {
	l, _ := newLimiter(t, time.Second)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := l.Allow(ctx, "k", 3, time.Minute); err != nil {
			t.Fatalf("request %d should be allowed: %v", i+1, err)
		}
	}
}

func TestAllow_ExceedsLimit(t *testing.T) {
	l, _ := newLimiter(t, time.Second)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_ = l.Allow(ctx, "k", 3, time.Minute)
	}
	err := l.Allow(ctx, "k", 3, time.Minute)
	if !errors.Is(err, ratelimit.ErrLimitExceeded) {
		t.Fatalf("expected ErrLimitExceeded, got %v", err)
	}
}

func TestAllow_IndependentKeys(t *testing.T) {
	l, _ := newLimiter(t, time.Second)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_ = l.Allow(ctx, "a", 3, time.Minute)
	}
	if err := l.Allow(ctx, "b", 3, time.Minute); err != nil {
		t.Fatalf("key b should be independent: %v", err)
	}
}

// The window must slide because the script trims old buckets by bucket index, not because the whole key expires.
// mr.SetTime moves only the clock the script reads through redis TIME; key TTLs do not move with it, so the hash is
// still there after the jump and an allow can only come from old buckets being trimmed. (mr.FastForward would do the
// opposite: expire the whole hash and leave TIME alone, hiding a wrong bucket trim.)
func TestAllow_WindowSlides(t *testing.T) {
	l, mr := newLimiter(t, 100*time.Millisecond)
	ctx := context.Background()
	now := time.Now()
	mr.SetTime(now)
	const window = 300 * time.Millisecond
	for i := 0; i < 3; i++ {
		if err := l.Allow(ctx, "k", 3, window); err != nil {
			t.Fatalf("request %d should be allowed: %v", i+1, err)
		}
	}
	if err := l.Allow(ctx, "k", 3, window); !errors.Is(err, ratelimit.ErrLimitExceeded) {
		t.Fatalf("in-window request should be limited, got %v", err)
	}
	mr.SetTime(now.Add(350 * time.Millisecond))
	if err := l.Allow(ctx, "k", 3, window); err != nil {
		t.Fatalf("after window slides should be allowed: %v", err)
	}
}

// Storage follows only the number of buckets and is independent of limit: multiplying limit by ten thousand leaves
// the field count unchanged. This is the whole reason the ZSET approach was replaced, so it needs a test.
func TestStorageIndependentOfLimit(t *testing.T) {
	l, mr := newLimiter(t, 100*time.Millisecond)
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		if err := l.Allow(ctx, "k", 100000, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	// window/bucket = 10 buckets; the 50 requests land in the first few buckets, so there are far fewer fields than
	// requests.
	fields, err := mr.HKeys("k")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(fields); got > 10 {
		t.Fatalf("hash has %d fields, want <= 10 (bucket count); storage must not scale with request count", got)
	}
}

// A window that is not a whole number of buckets (shorter than one included) would be counted over a different span,
// so it fails on the spot.
func TestAllow_WindowNotMultipleOfBucket(t *testing.T) {
	l, _ := newLimiter(t, time.Minute)
	for _, w := range []time.Duration{time.Second, 90 * time.Second} {
		if err := l.Allow(context.Background(), "k", 3, w); err == nil || !strings.Contains(err.Error(), "kredis.ratelimit-window") {
			t.Errorf("window %v: got %v, want kredis.ratelimit-window", w, err)
		}
	}
}

// A limit below 1 would reject everything; it fails instead.
func TestAllow_LimitBelowOne(t *testing.T) {
	l, _ := newLimiter(t, time.Minute)
	for _, n := range []int{0, -1} {
		if err := l.Allow(context.Background(), "k", n, time.Minute); err == nil || !strings.Contains(err.Error(), "kredis.ratelimit-limit") {
			t.Errorf("limit %d: got %v, want kredis.ratelimit-limit", n, err)
		}
	}
}

func TestNew_InvalidBucket(t *testing.T) {
	if _, err := ratelimit.New(nil, 0); err == nil {
		t.Fatal("non-positive bucket must fail")
	}
}
