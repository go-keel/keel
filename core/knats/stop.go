package knats

import (
	"context"
	"sync"
	"sync/atomic"
)

// gracefulStop is the pattern "stopping is started only once, but each waiter waits on its own terms".
//
// The intuitive version puts the whole blocking flow inside sync.Once.Do, so a later caller blocks on the Once until
// the first one finishes, which makes its own shorter budget (kratos's stopTimeout, for example) meaningless. So the
// Once only starts the stop, completion is signalled by a channel, and each caller selects independently.
//
// The three layers client (connection), consumer (one subscription) and consumerServer (a set of subscriptions) use
// the same mechanism, extracted here so that changing one place cannot leave another drifting.
type gracefulStop struct {
	once sync.Once
	// finished is closed after the stop flow completes.
	finished chan struct{}
	// abandoned records whether the stop was given up halfway; reporting success after giving up would tell the caller
	// falsely that all messages were handled cleanly.
	abandoned atomic.Bool
}

func newGracefulStop() gracefulStop {
	return gracefulStop{finished: make(chan struct{})}
}

// run starts the stop (at most once) and waits for it within the caller's budget, **returning whether it completed
// cleanly**.
//
// before runs **synchronously** inside the Once, for state that must take effect immediately; fn is the actual stop
// flow, started in the background with the budget from budget.
//
// The result must be returned from here; a caller must not read abandoned itself after run: on the ctx-expired path run
// returns before the background flow does, abandoned is not yet written and the read sees a stale value. Only the
// finished path has a happens-before edge (close comes after fn returns), so only there is the read accurate.
func (g *gracefulStop) run(ctx context.Context, before func(), fn func(context.Context)) bool {
	g.once.Do(func() {
		if before != nil {
			before()
		}
		go func() {
			defer close(g.finished)
			budget, cancel := stopBudget(ctx)
			defer cancel()
			fn(budget)
		}()
	})
	select {
	case <-g.finished:
		return !g.abandoned.Load()
	case <-ctx.Done():
		// The background flow may still be running. To the caller, "not finished within my budget" is not clean, and
		// is reported as such.
		return false
	}
}

// stopBudget takes the stop budget from the caller's ctx: it **keeps its deadline** but ignores its cancel, because
// when the first caller lets go the background flow still has to finish for the other callers. Only without a deadline
// does it fall back to a default, since waiting without limit on a stuck handler would never stop.
func stopBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(ctx)
	if dl, ok := ctx.Deadline(); ok {
		return context.WithDeadline(base, dl)
	}
	return context.WithTimeout(base, drainTimeout)
}
