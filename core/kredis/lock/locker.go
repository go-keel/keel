package lock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/internal/errs"
)

var ErrLockNotAcquired = errors.New("lock not acquired")
var ErrLockNotHeld = errors.New("lock not held")

// Locker is a distributed lock.
//
// **Design constraint: the work done while holding the lock must finish within ttlSec.** This implementation does
// not renew (there is no watchdog): the lock is released automatically when it expires, after which any instance
// can take the same lock. Callers set ttlSec from the worst-case duration of the work, erring on the generous side;
// overrunning means two processes enter the critical section at the same time, not that one waits in line.
//
// This is a deliberate trade-off: a renewal watchdog has to handle a chain of edge cases such as failed renewals,
// clock drift and GC pauses, and when those go wrong the symptom (believing the lock is held when it is already
// lost) is harder to debug than "estimate the TTL according to the constraint". For work that needs long
// transaction protection, use a database row lock or a business idempotency key instead; do not add renewal here.
type Locker interface {
	// TryAcquireLock tries to acquire the lock for key and returns a release function on success.
	// The work done while holding the lock must finish within ttlSec; see the design constraint on Locker.
	TryAcquireLock(ctx context.Context, key string, ttlSec int) (func(), error)
	// TryAcquireLockWithTimeout is like TryAcquireLock but retries until timeout when the lock is taken.
	TryAcquireLockWithTimeout(ctx context.Context, key string, ttlSec int, timeout time.Duration) (func(), error)
}

// NewLocker creates a new instance of Locker using the provided Redis client.
// It accepts redis.UniversalClient (consistent with ratelimit.New): *kredis.Redis embeds that interface and can be
// passed directly, as can a bare *redis.Client, a cluster client or a mock.
func NewLocker(rdb redis.UniversalClient) Locker {
	return &redisLocker{rdb: rdb}
}

type redisLocker struct {
	rdb redis.UniversalClient
}

func (r *redisLocker) TryAcquireLock(ctx context.Context, key string, ttlSec int) (func(), error) {
	// With ttlSec==0 SetNX sets no expiry, so the lock never expires and cannot self-heal via TTL after a crash.
	if ttlSec <= 0 {
		return nil, errs.Rules.New("kredis.lock-ttl", fmt.Sprintf("TryAcquireLock was called with ttlSec %d", ttlSec), "pass a positive ttlSec so a crashed holder's lock expires")
	}
	token := uuid.NewString()
	ok, err := r.rdb.SetNX(ctx, key, token, time.Duration(ttlSec)*time.Second).Result()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrLockNotAcquired
	}
	return func() {
		// ErrLockNotHeld means the lock expired before the work finished and someone else has taken it, i.e. the
		// Locker TTL constraint was violated and the critical section was actually entered concurrently. This is a
		// correctness incident, not tolerable jitter, so it is logged as Error; the release function has no error
		// return (callers write defer release()), so the log is the only way to report it.
		if err := r.releaseLock(key, token); err != nil {
			slog.Error("lock release failed; critical section may have overlapped",
				"key", key, "ttl_exceeded", errors.Is(err, ErrLockNotHeld), "error", err)
		}
	}, nil
}

const lockRetryInterval = 100 * time.Millisecond

func (r *redisLocker) TryAcquireLockWithTimeout(ctx context.Context, key string, ttlSec int, timeout time.Duration) (func(), error) {
	deadline := time.Now().Add(timeout)
	timer := time.NewTimer(lockRetryInterval)
	defer timer.Stop()
	for {
		release, err := r.TryAcquireLock(ctx, key, ttlSec)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, ErrLockNotAcquired) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, ErrLockNotAcquired
		}
		// Honor ctx cancellation while waiting to retry, so the loop does not spin until the deadline after the
		// caller has given up.
		timer.Reset(lockRetryInterval)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

var releaseScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
else
	return 0
end
`)

func (r *redisLocker) releaseLock(key string, token string) error {
	// release uses a separate background ctx with a short timeout: the caller's ctx may already be canceled, but
	// the lock must still be released, otherwise it is only returned when the TTL expires.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := releaseScript.Run(ctx, r.rdb, []string{key}, token).Int64()
	if err != nil {
		return err
	}
	if result == 0 {
		return ErrLockNotHeld
	}
	return nil
}
