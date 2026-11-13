package lock_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/core/kredis"
	"github.com/go-keel/keel/core/kredis/lock"
)

func newLocker(t *testing.T) lock.Locker {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return lock.NewLocker(&kredis.Redis{UniversalClient: rdb})
}

func TestTryAcquireLock_AcquireAndRelease(t *testing.T) {
	l := newLocker(t)
	ctx := context.Background()

	release, err := l.TryAcquireLock(ctx, "k", 30)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Acquiring again while held fails.
	if _, err := l.TryAcquireLock(ctx, "k", 30); !errors.Is(err, lock.ErrLockNotAcquired) {
		t.Errorf("second acquire should fail with ErrLockNotAcquired, got %v", err)
	}
	// After release it can be acquired again.
	release()
	release2, err := l.TryAcquireLock(ctx, "k", 30)
	if err != nil {
		t.Errorf("acquire after release should succeed: %v", err)
	}
	release2()
}

// Canceling ctx must interrupt the retry wait immediately instead of spinning until the deadline.
func TestTryAcquireLockWithTimeout_RespectsCtxCancel(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	l := lock.NewLocker(&kredis.Redis{UniversalClient: rdb})
	// Hold the lock first so the retry-with-timeout enters its wait loop.
	hold, err := l.TryAcquireLock(context.Background(), "k", 30)
	if err != nil {
		t.Fatalf("prime hold: %v", err)
	}
	defer hold()

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel right after the first failed attempt, so the cancellation lands while the locker waits to retry.
	rdb.AddHook(cancelAfterCommand{cancel})

	start := time.Now()
	// timeout is 10s but ctx is canceled after the first attempt: it should return right away, long before 10s.
	_, err = l.TryAcquireLockWithTimeout(ctx, "k", 30, 10*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("should return promptly on ctx cancel, took %v", elapsed)
	}
}

func TestTryAcquireLockWithTimeout_Timeout(t *testing.T) {
	l := newLocker(t)
	hold, _ := l.TryAcquireLock(context.Background(), "k", 30)
	defer hold()

	start := time.Now()
	_, err := l.TryAcquireLockWithTimeout(context.Background(), "k", 30, 300*time.Millisecond)
	if !errors.Is(err, lock.ErrLockNotAcquired) {
		t.Errorf("expected ErrLockNotAcquired on timeout, got %v", err)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Error("should retry until near the deadline")
	}
}

// With ttlSec<=0 go-redis SetNX sets no expiry and the lock never expires: if the holder crashes, other instances
// can never take the lock. It must be rejected at the entry point.
func TestTryAcquireLock_NonPositiveTTL(t *testing.T) {
	l := newLocker(t)
	ctx := context.Background()
	for _, ttl := range []int{0, -1} {
		if _, err := l.TryAcquireLock(ctx, "k-ttl", ttl); err == nil {
			t.Errorf("ttlSec=%d should be rejected", ttl)
		}
	}
}

// A failed release (for example the lock was taken by someone else after the TTL expired) is a strong signal that
// the critical section may have been entered concurrently; it must not be dropped silently, at least one log line
// is required.
func TestRelease_FailureIsLogged(t *testing.T) {
	l := newLocker(t)
	release, err := l.TryAcquireLock(context.Background(), "k-log", 30)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	release() // normal release
	release() // lock no longer held: ErrLockNotHeld, must be observable
	if !strings.Contains(buf.String(), "lock release failed") {
		t.Errorf("failed release should be logged, got: %q", buf.String())
	}
}

// cancelAfterCommand cancels a context after every command the client runs.
type cancelAfterCommand struct{ cancel context.CancelFunc }

func (h cancelAfterCommand) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h cancelAfterCommand) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		h.cancel()
		return err
	}
}

func (h cancelAfterCommand) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
