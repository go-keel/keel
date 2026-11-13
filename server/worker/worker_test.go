package worker_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-keel/keel/server/worker"
)

// kratos App.Run calls Start/Stop from two concurrent goroutines, and Stop may run first.
// The cancellation must not be lost then: Start must finish promptly rather than let the runner run until process exit.
//
// Stop waits for the runner to really exit before returning, so the test cannot call Stop synchronously first and
// then start Start: that would hang inside Stop (done not yet closed, and ctx is Background). The two must run
// concurrently, matching how kratos really calls them.
func TestWorker_StopBeforeStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := worker.Server(func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})

		stopDone := make(chan error, 1)
		go func() { stopDone <- w.Stop(context.Background()) }()
		synctest.Wait() // Stop is now waiting for the runner to exit

		startDone := make(chan error, 1)
		go func() { startDone <- w.Start(context.Background()) }()

		select {
		case err := <-startDone:
			_ = err // only that Start returns matters here, not its error
		case <-time.After(2 * time.Second):
			t.Fatal("Start did not return after prior Stop — cancellation lost")
		}
		select {
		case err := <-stopDone:
			if err != nil {
				t.Fatalf("stop: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Stop did not return after Start finished")
		}
	})
}

// Not exiting by the deadline must be reported as an error, not success: the caller would believe the runner finished cleanly.
func TestWorker_StopReturnsErrorWhenRunnerIgnoresCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		blocked := make(chan struct{})
		defer close(blocked)

		w := worker.Server(func(ctx context.Context) error {
			<-blocked // a runner that ignores ctx.Done()
			return nil
		})
		go func() { _ = w.Start(context.Background()) }()
		synctest.Wait() // the runner is running

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if err := w.Stop(ctx); err == nil {
			t.Fatal("runner did not exit but Stop reported success")
		}
	})
}

// Together with -race, covers synchronisation of concurrent Start/Stop access to the cancel field.
func TestWorker_ConcurrentStartStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := worker.Server(func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		})
		done := make(chan struct{})
		go func() {
			_ = w.Start(context.Background())
			close(done)
		}()
		synctest.Wait() // the runner is running
		if err := w.Stop(context.Background()); err != nil {
			t.Fatalf("stop: %v", err)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Start did not return after Stop")
		}
	})
}
