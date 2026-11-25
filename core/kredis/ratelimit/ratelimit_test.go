package ratelimit_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/core/kredis/ratelimit"
)

// newOfflineLimiter builds a limiter whose client never connects: the checks below run before any command.
func newOfflineLimiter(t *testing.T, bucket time.Duration) *ratelimit.Limiter {
	t.Helper()
	l, err := ratelimit.New(redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}), bucket)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// A window that is not a whole number of buckets (shorter than one included) would be counted over a different span,
// so it fails on the spot.
func TestAllow_WindowNotMultipleOfBucket(t *testing.T) {
	l := newOfflineLimiter(t, time.Minute)
	for _, w := range []time.Duration{time.Second, 90 * time.Second} {
		if err := l.Allow(context.Background(), "k", 3, w); err == nil || !strings.Contains(err.Error(), "kredis.ratelimit-window") {
			t.Errorf("window %v: got %v, want kredis.ratelimit-window", w, err)
		}
	}
}

// A limit below 1 would reject everything; it fails instead.
func TestAllow_LimitBelowOne(t *testing.T) {
	l := newOfflineLimiter(t, time.Minute)
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
