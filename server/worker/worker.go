package worker

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"

	"github.com/go-keel/keel"
)

type Runner func(ctx context.Context) error

// Server wraps a long-running Runner as a keel.Server so it joins the app
// lifecycle (graceful start/stop).
func Server(runner Runner) keel.Server {
	return &workerImpl{
		cancel: func() {},
		runner: runner,
		// done is closed after the runner returns; Stop uses it to tell whether the runner has really exited.
		// It is initialised here rather than in Start: the order of kratos's two goroutines is not fixed, Stop
		// may arrive first, and done must already exist then, otherwise it is a nil channel.
		done: make(chan struct{}),
	}
}

type workerImpl struct {
	// kratos App.Run calls Start/Stop from two concurrent goroutines; mu guards installing and reading cancel.
	// stopped records "Stop arrived first", so Start cancels immediately, preventing a cancellation lost in the
	// race window from leaving the runner running forever.
	mu      sync.Mutex
	cancel  context.CancelFunc
	stopped bool
	runner  Runner
	done    chan struct{}
}

// Start runs the Runner, recovering from panics. A panic is converted to an
// error so kratos triggers graceful shutdown instead of crashing the process.
func (w *workerImpl) Start(ctx context.Context) (err error) {
	defer close(w.done)
	runCtx, cancel := context.WithCancel(ctx)
	w.mu.Lock()
	w.cancel = cancel
	stopped := w.stopped
	w.mu.Unlock()
	if stopped {
		cancel()
	}
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "worker runner panic recovered",
				"panic", r,
				"stack", string(debug.Stack()),
			)
			err = fmt.Errorf("worker runner panic: %v", r)
		}
	}()
	return w.runner(runCtx)
}

// Stop cancels the Runner's ctx and waits for it to really exit within the caller's budget (the deadline kratos
// derives from app.stop_timeout). If it has not exited by then, Stop returns an error truthfully instead of reporting
// success and letting the caller believe it stopped cleanly.
//
// Remaining limit: kratos's App.Run waits with an errgroup for Start to return before this server counts as stopped
// (eg.Wait()); the wait timeout here only makes Stop's return value honest. If the Runner ignores ctx.Done()
// entirely, Start never returns and the process still cannot stop. The framework cannot guarantee this layer; it can
// only require that the Runner watches ctx.
func (w *workerImpl) Stop(ctx context.Context) error {
	w.mu.Lock()
	w.stopped = true
	cancel := w.cancel
	done := w.done
	w.mu.Unlock()
	cancel()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("worker: runner did not exit within the stop budget: %w", ctx.Err())
	}
}
