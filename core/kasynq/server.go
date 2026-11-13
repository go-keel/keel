package kasynq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"

	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/core/internal/msgtrace"
	"github.com/go-keel/keel/core/kconfig"
	"github.com/go-keel/keel/internal/errs"
)

// WorkerSetup registers task handlers into a WorkerServer.
type WorkerSetup func(r HandlerRegistrar)

// HandlerRegistrar is the registration interface exposed to WorkerSetup.
type HandlerRegistrar interface {
	// HandleFunc is the bytes-level registration entry point: the payload is passed to fn as is, without decoding.
	// Ordinary business code uses TaskType.Handle (which goes through it); this one is for custom encodings.
	HandleFunc(taskType string, fn func(ctx context.Context, payload []byte) error)
}

// WorkerServer is an asynq-backed async task consumer that implements transport.Server.
type WorkerServer struct {
	rdb redis.UniversalClient
	srv *asynq.Server
	mux *asynq.ServeMux
	// err accumulates HandleFunc's registration errors (empty task name, duplicate registration). asynq's
	// ServeMux.Handle panics on these, so they are recovered into errors recorded on the server and returned at once
	// by NewNamedWorkerServer after setup runs, the same shape as event subscription and cron registration: the
	// registration callback itself returns no error.
	err error
}

// NewWorkerServer builds the worker from the asynq.default config section; see NewNamedWorkerServer.
func NewWorkerServer(c *kconfig.Config, rdb redis.UniversalClient, setup WorkerSetup) (*WorkerServer, error) {
	return NewNamedWorkerServer(c, rdb, "default", setup)
}

// NewNamedWorkerServer builds the worker from the asynq.<name> config section (an empty name means "default"). A
// missing section is an error, and registration errors from setup are returned at once.
func NewNamedWorkerServer(c *kconfig.Config, rdb redis.UniversalClient, name string, setup WorkerSetup) (*WorkerServer, error) {
	if name == "" {
		name = "default"
	}
	tc, err := getAsynqConfig(c, name)
	if err != nil {
		return nil, err
	}
	// asynq's own shutdown deadline (Config.ShutdownTimeout) is derived from app.stop_timeout, so there is only one
	// number to configure; see the comment in buildAsynqConfig.
	cfg := buildAsynqConfig(tc, c.StopTimeout())
	srv := asynq.NewServerFromRedisClient(rdb, cfg)
	mux := asynq.NewServeMux()
	// otel is outermost and panicRecovery sits next to the handler: a panic recovered into an error still flows
	// through the otel middleware, so the span is marked Error. The cost is that a panic in the otel middleware
	// itself is no longer recovered (its implementation has no panic path, which is acceptable).
	mux.Use(otelTaskMiddleware)
	mux.Use(panicRecoveryMiddleware)

	w := &WorkerServer{rdb: rdb, srv: srv, mux: mux}
	if setup != nil {
		setup(w)
	}
	if w.err != nil {
		return nil, w.err
	}
	return w, nil
}

// Mux returns the underlying asynq ServeMux, so users can register handlers with the native signature or add their
// own middleware.
//
// **Handlers registered here are still protected by kasynq's middleware**: asynq applies mux.mws only at dispatch
// (ServeMux.Handler), so the otel span and panic recovery still take effect. The only difference from HandleFunc is
// the signature: the handler gets an *asynq.Task rather than the payload bytes, and duplicate registration errors come
// from asynq itself.
func (w *WorkerServer) Mux() *asynq.ServeMux { return w.mux }

// Server returns the underlying asynq Server, so users can call native capabilities kasynq does not wrap.
func (w *WorkerServer) Server() *asynq.Server { return w.srv }

// Start checks the redis eviction policy and starts processing tasks.
func (w *WorkerServer) Start(ctx context.Context) error {
	if err := checkEvictionPolicy(ctx, w.rdb); err != nil {
		return err
	}
	return w.srv.Start(w.mux)
}

// Stop bounds asynq's Shutdown by the ctx budget. asynq.Server.Shutdown takes no ctx and honours only
// asynq.Config.ShutdownTimeout from construction, which NewNamedWorkerServer has already set to a value derived from
// app.stop_timeout (see buildAsynqConfig). Normally asynq therefore puts in-flight tasks back on the queue and
// finishes on its own when that value expires, before the ctx here does, and the select below reliably takes the
// <-done branch. The ctx layer is a second safeguard that steps in only when asynq's internal deadline cannot cope
// either (for example, broker.Close hangs): it stops waiting at the deadline and honestly returns an error instead
// of letting the caller believe shutdown was clean.
//
// Remaining limitation: even when asynq's own shutdown deadline expires and in-flight tasks are forcibly put back on
// the queue, asynq.Server.Shutdown does not report it (it has no return value). Stop cannot get the signal "were any
// tasks forcibly interrupted"; returning nil only means "asynq considers its wrap-up done", not "every task finished
// normally within the budget". This is a limit of the upstream API, and this layer cannot give a stronger
// guarantee.
func (w *WorkerServer) Stop(ctx context.Context) error {
	done := make(chan struct{})
	go func() { w.srv.Shutdown(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// HandleFunc registers a task handler (bytes-level; see the HandlerRegistrar interface comment).
//
// Registering the same taskType twice (or an empty task name) should be a startup error, but asynq's
// ServeMux.Handle panics on these. The panic is recovered into an error recorded on w.err and returned at once by
// NewNamedWorkerServer after setup runs, the same shape as the other three registrations (raw routes, event
// subscriptions, cron): no panic, nothing silent.
func (w *WorkerServer) HandleFunc(taskType string, fn func(ctx context.Context, payload []byte) error) {
	defer func() {
		if r := recover(); r != nil {
			w.err = errors.Join(w.err, errs.Rules.New("kasynq.register-failed", fmt.Sprintf("registering task handler %q failed: %v", taskType, r), "give each handler a non-empty task type and register each type once"))
		}
	}()
	w.mux.HandleFunc(taskType, func(ctx context.Context, t *asynq.Task) error {
		return fn(ctx, t.Payload())
	})
}

// panicRecoveryMiddleware recovers from panics in task handlers, converting
// them to errors so asynq applies its retry policy instead of crashing the
// worker process.
func panicRecoveryMiddleware(next asynq.Handler) asynq.Handler {
	return asynq.HandlerFunc(func(ctx context.Context, t *asynq.Task) (err error) {
		defer func() {
			if r := recover(); r != nil {
				slog.ErrorContext(ctx, "task worker panic recovered",
					"panic", r,
					"task_type", t.Type(),
					"stack", string(debug.Stack()),
				)
				err = fmt.Errorf("panic in task handler [%s]: %v", t.Type(), r)
			}
		}()
		return next.ProcessTask(ctx, t)
	})
}

func otelTaskMiddleware(next asynq.Handler) asynq.Handler {
	return asynq.HandlerFunc(func(ctx context.Context, t *asynq.Task) error {
		queueName, _ := asynq.GetQueueName(ctx)
		// StartReceive continues the traceparent injected on the enqueue side (see TaskClient.Enqueue). A task
		// fired by cron has no upstream ctx, so its headers are empty, Extract is a no-op and a new trace starts as
		// usual. task.type is a task-specific attribute and is still added.
		ctx, span := msgtrace.StartReceive(ctx, "asynq", "task.process", queueName,
			propagation.MapCarrier(t.Headers()), attribute.String("task.type", t.Type()))
		defer span.End()
		err := next.ProcessTask(ctx, t)
		if err != nil {
			// Retries and give-ups are treated alike and both marked failed: whether asynq retries or archives
			// directly is decided by settings such as MaxRetry, and is not distinguished here.
			msgtrace.MarkFailed(span, err)
		}
		return err
	})
}
