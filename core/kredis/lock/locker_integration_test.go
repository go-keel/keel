//go:build integration

package lock_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/core/kredis/lock"
	"github.com/go-keel/keel/internal/integrationtest"
)

func newClient(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: integrationtest.Env(t, "TEST_REDIS_ADDR")})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func newLocker(t *testing.T) lock.Locker {
	t.Helper()
	return lock.NewLocker(newClient(t))
}

// testKey is unique per test and run, so runs against the same server do not see each other's locks.
func testKey(t *testing.T) string {
	return fmt.Sprintf("it:lock:%s:%d", t.Name(), time.Now().UnixNano())
}

func TestTryAcquireLock_AcquireAndRelease(t *testing.T) {
	l := newLocker(t)
	ctx := context.Background()
	k := testKey(t)

	release, err := l.TryAcquireLock(ctx, k, 30)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Acquiring again while held fails.
	if _, err := l.TryAcquireLock(ctx, k, 30); !errors.Is(err, lock.ErrLockNotAcquired) {
		t.Errorf("second acquire should fail with ErrLockNotAcquired, got %v", err)
	}
	// After release it can be acquired again.
	release()
	release2, err := l.TryAcquireLock(ctx, k, 30)
	if err != nil {
		t.Errorf("acquire after release should succeed: %v", err)
	}
	release2()
}

// Canceling ctx must interrupt the retry wait immediately instead of spinning until the deadline.
func TestTryAcquireLockWithTimeout_RespectsCtxCancel(t *testing.T) {
	rdb := newClient(t)
	l := lock.NewLocker(rdb)
	k := testKey(t)
	// Hold the lock first so the retry-with-timeout enters its wait loop.
	hold, err := l.TryAcquireLock(context.Background(), k, 30)
	if err != nil {
		t.Fatalf("prime hold: %v", err)
	}
	defer hold()

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel right after the first failed attempt, so the cancellation lands while the locker waits to retry.
	rdb.AddHook(cancelAfterCommand{cancel})

	start := time.Now()
	// timeout is 10s but ctx is canceled after the first attempt: it should return right away, long before 10s.
	_, err = l.TryAcquireLockWithTimeout(ctx, k, 30, 10*time.Second)
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
	k := testKey(t)
	hold, _ := l.TryAcquireLock(context.Background(), k, 30)
	defer hold()

	start := time.Now()
	_, err := l.TryAcquireLockWithTimeout(context.Background(), k, 30, 300*time.Millisecond)
	if !errors.Is(err, lock.ErrLockNotAcquired) {
		t.Errorf("expected ErrLockNotAcquired on timeout, got %v", err)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Error("should retry until near the deadline")
	}
}

// A failed release (for example the lock was taken by someone else after the TTL expired) is a strong signal that
// the critical section may have been entered concurrently; it must not be dropped silently, at least one log line
// is required.
func TestRelease_FailureIsLogged(t *testing.T) {
	l := newLocker(t)
	release, err := l.TryAcquireLock(context.Background(), testKey(t), 30)
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
