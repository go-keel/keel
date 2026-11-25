package lock_test

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/core/kredis/lock"
)

// With ttlSec<=0 go-redis SetNX sets no expiry and the lock never expires: if the holder crashes, other instances
// can never take the lock. It must be rejected at the entry point.
func TestTryAcquireLock_NonPositiveTTL(t *testing.T) {
	// The check runs before any command, so the client never connects.
	l := lock.NewLocker(redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}))
	ctx := context.Background()
	for _, ttl := range []int{0, -1} {
		if _, err := l.TryAcquireLock(ctx, "k-ttl", ttl); err == nil {
			t.Errorf("ttlSec=%d should be rejected", ttl)
		}
	}
}
