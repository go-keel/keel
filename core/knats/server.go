package knats

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/go-keel/keel"
	"github.com/go-keel/keel/internal/errs"
)

// NewConsumerServer wraps a set of consumers as a keel.Server, bringing them into the application lifecycle.
//
// Subscriptions are all registered at once through setup when the server is built (ADR keel-0005, "Rejected
// approaches"), so the problem of consumers registered too late cannot occur. Register inside setup returns no error;
// validation failures are recorded on the registrar and taken out and returned together here after setup runs, the
// same shape as the two other registrations outside event contracts (kasynq's task handlers and cron).
//
// Stop stops only the subscriptions held here and does not touch the connection: the connection's lifetime belongs
// entirely to the cleanup function NewClient returns, and the two do not interfere. After this server stops, other code
// in the same process holding the Client can still publish normally.
func NewConsumerServer(cl *Client, setup ConsumerSetup) (keel.Server, error) {

	r := &consumerRegistrar{c: cl}
	if setup != nil {
		setup(r)
	}
	if r.err != nil {
		return nil, r.err
	}
	if len(r.consumers) == 0 {
		return nil, errs.Rules.Wrap(ErrNoConsumers, "knats.no-consumers", "NewConsumerServer's setup registered no consumers", "call Register in the ConsumerSetup, or drop the consumer server from di.go")
	}

	return &consumerServer{
		consumers: r.consumers,
		failures:  make(chan error, 1),
		stopping:  make(chan struct{}),
		stopper:   newGracefulStop(),
	}, nil
}

type consumerServer struct {
	consumers []*consumer
	// failures receives the asynchronous death of a consumer; see the consumer.watch comment. After the server
	// terminates a subscription (durable deleted, consumer kicked), nats.go merely stops the subscription: the process
	// stays alive, health checks keep passing and messages pile up silently. It must be pushed back to Start so the
	// whole server exits with an error and the orchestrator restarts it.
	failures chan error
	// stopping is closed when Stop is called, waking the select in Start. The subscriptions' stop signal and the
	// connection's close signal (client.closed) are two independent events and cannot share one channel, so the server
	// needs its own signal.
	stopping chan struct{}
	stopper  gracefulStop
}

func (s *consumerServer) Start(ctx context.Context) error {
	for i, cs := range s.consumers {
		if err := cs.start(ctx, s.failures); err != nil {
			for _, started := range s.consumers[:i] {
				_ = started.stop(ctx)
			}
			return err
		}
	}

	select {
	case <-ctx.Done():
		return nil
	case <-s.stopping:
		return nil
	case err := <-s.failures:
		// Any consumer dying asynchronously must take the whole server down: a live process whose messages nobody
		// consumes is the hardest kind of failure to notice.
		return err
	}
}

// Stop stops only the subscriptions this server holds and does not touch the connection (ADR keel-0005, "Only the
// cleanup function closes the connection"). The stop budget comes entirely from ctx, the deadline kratos derives from
// app.stop_timeout; kratos always provides that deadline, so setting another one is unnecessary.
func (s *consumerServer) Stop(ctx context.Context) error {
	ok := s.stopper.run(ctx, func() { close(s.stopping) }, func(budget context.Context) {
		// Stop in parallel: serially the worst case is the drain limit × the number of consumers, and the ones queued
		// later keep pulling messages while they wait. The result comes from stop's return value, not from each
		// consumer's abandoned, which may not be written yet.
		var dirty atomic.Bool
		var wg sync.WaitGroup
		for _, cs := range s.consumers {
			wg.Go(func() {
				if !cs.stop(budget) {
					dirty.Store(true)
				}
			})
		}
		wg.Wait() // happens-before the Store above, so the read below is safe
		if dirty.Load() {
			s.stopper.abandoned.Store(true)
		}
	})
	if ok {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("knats: consumer server stop gave up waiting; some messages fall back to redelivery")
}
