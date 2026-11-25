package lock_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/core/kredis/lock"
	"github.com/go-keel/keel/pkg/rule"
)

// A TTL under 1ms is rejected before any command: zero or negative would set no expiry, so a crashed holder keeps the
// lock forever, and a bare integer such as 30 is 30ns, a lock gone before the work starts.
func TestTryAcquireLock_TTLUnderOneMillisecond(t *testing.T) {
	// The check runs before any command, so the client never connects.
	l := lock.NewLocker(redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}))
	ctx := context.Background()
	for _, ttl := range []time.Duration{0, -time.Second, 30, time.Millisecond - 1} {
		_, err := l.TryAcquireLock(ctx, "k-ttl", ttl)
		if re, ok := errors.AsType[*rule.Error](err); !ok || re.ID != "kredis.lock-ttl" {
			t.Errorf("ttl %v: err = %v, want kredis.lock-ttl", ttl, err)
		}
	}
}
