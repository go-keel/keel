package lock

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/internal/errs"
	"github.com/go-keel/keel/internal/redislease"
)

var ErrLockNotAcquired = errors.New("lock not acquired")
var ErrLockNotHeld = errors.New("lock not held")

// minTTL is the shortest lock TTL accepted. Redis expires keys with millisecond precision, and a bare integer passed
// as a time.Duration (30 meaning seconds) is 30 nanoseconds: rejecting it keeps such a call from taking a lock that
// is gone before the work starts.
const minTTL = time.Millisecond

// Locker is a distributed lock.
//
// **Design constraint: the work done while holding the lock must finish within ttl.** This implementation does
// not renew (there is no watchdog): the lock is released automatically when it expires, after which any instance
// can take the same lock. Callers set ttl from the worst-case duration of the work, erring on the generous side;
// overrunning means two processes enter the critical section at the same time, not that one waits in line.
//
// This is a deliberate trade-off: a renewal watchdog has to handle a chain of edge cases such as failed renewals,
// clock drift and GC pauses, and when those go wrong the symptom (believing the lock is held when it is already
// lost) is harder to debug than "estimate the TTL according to the constraint". For work that needs long
// transaction protection, use a database row lock or a business idempotency key instead; do not add renewal here.
//
// Code that needs a test double defines its own small interface with the methods it calls.
type Locker struct {
	rdb redis.UniversalClient
}

// NewLocker creates a Locker on the given Redis client. It accepts redis.UniversalClient (consistent with
// ratelimit.New): *kredis.Redis embeds that interface and can be passed directly, as can a bare *redis.Client or a
// cluster client.
func NewLocker(rdb redis.UniversalClient) *Locker {
	return &Locker{rdb: rdb}
}

// TryAcquireLock tries to acquire the lock for key and returns a release function on success; ErrLockNotAcquired means
// another holder has it. The work done while holding the lock must finish within ttl; see the design constraint on
// Locker. A ttl under 1ms is rejected with kredis.lock-ttl.
func (r *Locker) TryAcquireLock(ctx context.Context, key string, ttl time.Duration) (func(), error) {
	// Without a TTL SetNX sets no expiry, so the lock never expires and cannot self-heal after a crash.
	if ttl < minTTL {
		return nil, errs.Rules.New("kredis.lock-ttl", fmt.Sprintf("TryAcquireLock was called with ttl %v", ttl), "pass a time.Duration of at least 1ms that covers the locked work, such as 30*time.Second")
	}
	token := rand.Text()
	ok, err := r.rdb.SetNX(ctx, key, token, ttl).Result()
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

// TryAcquireLockWithTimeout is like TryAcquireLock but retries every 100ms until timeout when the lock is taken, and
// stops as soon as ctx is canceled.
func (r *Locker) TryAcquireLockWithTimeout(ctx context.Context, key string, ttl, timeout time.Duration) (func(), error) {
	deadline := time.Now().Add(timeout)
	timer := time.NewTimer(lockRetryInterval)
	defer timer.Stop()
	for {
		release, err := r.TryAcquireLock(ctx, key, ttl)
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

func (r *Locker) releaseLock(key string, token string) error {
	// Release runs on its own short timeout: the caller's ctx may already be canceled, but the lock must still be
	// released, otherwise it is only returned when the TTL expires.
	err := redislease.Release(r.rdb, key, token)
	if errors.Is(err, redislease.ErrNotHeld) {
		return ErrLockNotHeld
	}
	return err
}
