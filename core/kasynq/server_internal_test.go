// In-package: wraps a handler with the unexported tracing middleware to check trace propagation.

package kasynq

import (
	"context"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
)

func workerTestConfig() *kconfig.Config {
	return &kconfig.Config{Config: &conf.Config{Asynq: map[string]*conf.Asynq{"default": {}}}}
}

// Registering the same task name twice should be a startup error, but asynq's ServeMux.Handle panics on it. After
// recovery it must become NewNamedWorkerServer's return value instead of actually panicking into the caller, the
// same shape as event subscription and cron registration: "no panic, the constructor returns it at once".
func TestNewWorkerServer_DuplicateTaskNameReturnsError(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_, err := NewNamedWorkerServer(workerTestConfig(), rdb, "default", func(r HandlerRegistrar) {
		r.HandleFunc("dup", func(context.Context, []byte) error { return nil })
		r.HandleFunc("dup", func(context.Context, []byte) error { return nil })
	})
	if err == nil {
		t.Fatal("duplicate task registration should return an error, not panic")
	}
}

// One asynchronous business operation is one trace: the traceparent Enqueue injects into the headers must be
// continued on the consuming side, or producer and consumer are two isolated traces. Covers the whole chain of
// inject -> headers -> extract.
func TestTracePropagation_ConsumerJoinsProducerTrace(t *testing.T) {
	oldProp := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() { otel.SetTextMapPropagator(oldProp) })

	// producer side: the real SDK creates a valid span ctx, injected the way Enqueue does
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "biz")
	wantTrace := span.SpanContext().TraceID()
	headers := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, headers)
	span.End()
	if headers["traceparent"] == "" {
		t.Fatal("traceparent not injected into headers")
	}

	// consumer side: extracted through otelTaskMiddleware. The real SDK provider is required: the noop tracer's
	// Start does not keep the remote context, and in production kotel registers the SDK.
	oldTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(oldTP) })
	var gotCtx context.Context
	h := otelTaskMiddleware(asynq.HandlerFunc(func(ctx context.Context, _ *asynq.Task) error {
		gotCtx = ctx
		return nil
	}))
	task := asynq.NewTaskWithHeaders("x", nil, headers)
	if err := h.ProcessTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if got := trace.SpanContextFromContext(gotCtx).TraceID(); got != wantTrace {
		t.Errorf("consumer trace = %s, want %s (producer)", got, wantTrace)
	}
}

// A handler panic converted into an error by PanicRecovery must flow through the otel middleware and mark the span
// Error; otherwise the most severe kind of failure (a panic) would be the least visible in traces.
func TestWorkerServer_PanicMarksSpanError(t *testing.T) {
	span := &recordingSpan{}
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(&recordingProvider{tracer: &recordingTracer{span: span}})
	t.Cleanup(func() { otel.SetTracerProvider(old) })

	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	w, err := NewNamedWorkerServer(workerTestConfig(), rdb, "default", nil)
	if err != nil {
		t.Fatalf("new worker server: %v", err)
	}
	w.HandleFunc("boom", func(ctx context.Context, payload []byte) error { panic("kaboom") })

	if err := w.mux.ProcessTask(context.Background(), asynq.NewTask("boom", nil)); err == nil {
		t.Fatal("panic should be converted to error")
	}
	if span.status != codes.Error {
		t.Errorf("span status = %v, want Error", span.status)
	}
}

// The escape hatch's promise: a handler registered with the native signature through Mux() is still protected by
// kasynq's middleware. This relies on asynq applying mux.mws only at dispatch (ServeMux.Handler), an upstream
// implementation detail, so it needs a regression test: if that changes the promise no longer holds, and the docs
// are written against the promise.
func TestWorkerServer_MuxEscapeHatchKeepsMiddleware(t *testing.T) {
	span := &recordingSpan{}
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(&recordingProvider{tracer: &recordingTracer{span: span}})
	t.Cleanup(func() { otel.SetTracerProvider(old) })

	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	w, err := NewNamedWorkerServer(workerTestConfig(), rdb, "default", nil)
	if err != nil {
		t.Fatalf("new worker server: %v", err)
	}
	// register with the native signature, bypassing kasynq.HandleFunc
	w.Mux().HandleFunc("native", func(ctx context.Context, t *asynq.Task) error { panic("kaboom") })

	if err := w.mux.ProcessTask(context.Background(), asynq.NewTask("native", nil)); err == nil {
		t.Fatal("panic should still be recovered for natively registered handlers")
	}
	if span.status != codes.Error {
		t.Errorf("span status = %v, want Error; otel middleware must still apply", span.status)
	}
}
