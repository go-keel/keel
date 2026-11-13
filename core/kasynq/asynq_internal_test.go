// In-package: checks queue-name resolution and the unexported zero-option sentinel.

package kasynq

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

type recordingSpan struct {
	noop.Span
	status codes.Code
}

func (s *recordingSpan) SetStatus(c codes.Code, _ string) { s.status = c }

type recordingTracer struct {
	noop.Tracer
	span *recordingSpan
}

func (t *recordingTracer) Start(ctx context.Context, _ string, _ ...trace.SpanStartOption) (context.Context, trace.Span) {
	return trace.ContextWithSpan(ctx, t.span), t.span
}

type recordingProvider struct {
	noop.TracerProvider
	tracer *recordingTracer
}

func (p *recordingProvider) Tracer(string, ...trace.TracerOption) trace.Tracer { return p.tracer }

// TaskType.RegisterCron encodes the payload **before** it calls ScheduleRegistrar.Register. With the usage
// docs/tasks.md recommends (SchedulerSetup ignores RegisterCron's return value), an encoding failure must still
// be reported by NewSchedulerServer; otherwise the task is never registered while the server is built successfully
// (the encoding fails, yet the caller gets a non-nil server and a nil error).
func TestNewSchedulerServer_RegisterCronEncodeFailureSurfaces(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	badTask := NewTaskType[chan int]("test:unencodable") // a chan cannot be json.Marshal-ed

	_, err := NewSchedulerServer(schedulerTestConfig(), rdb, func(r ScheduleRegistrar) {
		_, _ = badTask.RegisterCron(r, "@every 1m", make(chan int)) // recommended usage: ignore the return value
	})
	if err == nil {
		t.Fatal("a payload encoding failure should make NewSchedulerServer return an error, even when setup ignores RegisterCron's return value")
	}
}

// queueNameOf only affects the span's messaging.destination.name, not actual enqueueing, but getting it wrong would
// make the destination seen in the trace differ from the queue asynq actually used, so it is pinned here.
func TestQueueNameOf(t *testing.T) {
	if got := queueNameOf(nil); got != "default" {
		t.Errorf("no Queue option: got %q, want default", got)
	}
	if got := queueNameOf([]EnqueueOption{MaxRetry(3)}); got != "default" {
		t.Errorf("unrelated option only: got %q, want default", got)
	}
	if got := queueNameOf([]EnqueueOption{Queue("critical")}); got != "critical" {
		t.Errorf("explicit queue: got %q, want critical", got)
	}
	if got := queueNameOf([]EnqueueOption{MaxRetry(3), Queue("low")}); got != "low" {
		t.Errorf("queue mixed with other options: got %q, want low", got)
	}
}

// A zero-value EnqueueOption is a programming error: asynq silently ignores a nil option, and the caller most likely
// meant to pass something that did not get through. It must be rejected before touching redis, consistently with
// cron registration.
func TestEnqueue_ZeroOptionRejected(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	tc := NewTaskClient(rdb)
	_, err := tc.Enqueue(context.Background(), "t", nil, EnqueueOption{})
	if !errors.Is(err, errZeroOption) {
		t.Fatalf("zero-value option should be rejected before dialing redis, got: %v", err)
	}
}

// If the span status were not set to Error on err, APM filtering by status would miss every enqueue failure.
func TestEnqueue_ErrorSetsSpanStatus(t *testing.T) {
	span := &recordingSpan{}
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(&recordingProvider{tracer: &recordingTracer{span: span}})
	t.Cleanup(func() { otel.SetTracerProvider(old) })

	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond})
	tc := NewTaskClient(rdb)

	if _, err := tc.Enqueue(context.Background(), "t", nil); err == nil {
		t.Fatal("enqueue should fail against unreachable redis")
	}
	if span.status != codes.Error {
		t.Errorf("span status = %v, want Error", span.status)
	}
}
