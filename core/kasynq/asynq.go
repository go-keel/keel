package kasynq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"

	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/core/internal/msgtrace"
	"github.com/go-keel/keel/internal/errs"
)

// TaskType binds a task name to its payload type in one definition and is the recommended entry point for both
// enqueueing and consuming:
//
//	var SendReminder = kasynq.NewTaskType[ReminderPayload]("note:send_reminder")
//	SendReminder.Enqueue(ctx, client, p, kasynq.MaxRetry(3))     // enqueue
//	SendReminder.Handle(r, func(ctx, p ReminderPayload) error)   // consume
//	SendReminder.RegisterCron(r, "0 8 * * *", p)                 // schedule
//
// A misspelled task name or a wrong payload type is caught at compile time, and it owns JSON encoding and decoding
// on both sides. Custom encodings and dynamic task names go through the bytes-level entry points TaskClient.Enqueue
// and HandleFunc, which are a supported public capability, not internal plumbing.
type TaskType[T any] struct {
	name string
}

// NewTaskType returns the TaskType for the task named name.
func NewTaskType[T any](name string) TaskType[T] { return TaskType[T]{name: name} }

// Enqueue encodes the payload and enqueues the task. asynq rejects an empty task name, so it is not validated again
// here.
//
// It takes an Enqueuer rather than a *TaskClient, so business tests pass a fake; *TaskClient satisfies Enqueuer.
func (t TaskType[T]) Enqueue(ctx context.Context, e Enqueuer, p T, opts ...EnqueueOption) (*TaskInfo, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("kasynq: encode %s payload: %w", t.name, err)
	}
	return e.Enqueue(ctx, t.name, b, opts...)
}

// Handle registers a strongly typed handler.
//
// The error returned when decoding fails already carries asynq.SkipRetry: a bad payload can never be decoded, so
// retrying it like an ordinary failure would only spin through MaxRetry attempts before archiving it. Here it goes
// straight to the archived state.
func (t TaskType[T]) Handle(r HandlerRegistrar, fn func(ctx context.Context, p T) error) {
	r.HandleFunc(t.name, func(ctx context.Context, payload []byte) error {
		var p T
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("kasynq: decode %s payload: %w", t.name, errors.Join(err, asynq.SkipRetry))
		}
		return fn(ctx, p)
	})
}

// RegisterCron puts the task on a cron schedule (cronspec is interpreted in UTC; see NewSchedulerServer). The
// returned entry ID is for unregistering it dynamically through the native Scheduler() escape hatch.
//
// An encoding failure happens before r.Register is called, so Register's internal error accumulation never sees it.
// SchedulerSetup itself returns no error, and when the return value here is ignored as the recommended usage does,
// an encoding failure would vanish entirely: the server is built successfully but the task is never registered.
// r.Fail closes that gap by feeding the same accumulation as Register.
func (t TaskType[T]) RegisterCron(r ScheduleRegistrar, cronspec string, p T, opts ...EnqueueOption) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		err = fmt.Errorf("kasynq: encode %s payload: %w", t.name, err)
		r.Fail(err)
		return "", err
	}
	return r.Register(cronspec, t.name, b, opts...)
}

// Enqueuer is the narrow interface needed to enqueue tasks; its only method is Enqueue.
//
// It is the seam for business tests: *TaskClient is concrete and also carries Client(), an escape hatch that returns
// a third-party raw type, so a fake replaces Enqueuer instead. The reason is the same as for knats.Publisher (ADR keel-0005, "Rejected approaches"). The biz
// layer depends on it, business fakes implement it, and TaskType.Enqueue takes it.
type Enqueuer interface {
	// Enqueue is the bytes-level enqueue entry point: the payload is enqueued as is, without encoding. Ordinary
	// business code uses TaskType.Enqueue (which goes through it); this one is for custom encodings and dynamic task
	// names, and is also what fakes implement in business tests.
	//
	// Enqueueing with TaskID returns ErrTaskIDConflict when a task with the same ID exists, and enqueueing with
	// Unique returns ErrDuplicateTask when a duplicate task exists. Both are commonly used for idempotency: a
	// collision means the task was already enqueued.
	Enqueue(ctx context.Context, typeName string, payload []byte, opts ...EnqueueOption) (*TaskInfo, error)
}

// ErrTaskIDConflict and ErrDuplicateTask are the two enqueue collision errors, one for each of the exposed options:
// TaskID(...) can hit ErrTaskIDConflict and Unique(...) can hit ErrDuplicateTask. Since Keel took over the options,
// it has to take over their errors too, or business code would have to import asynq just to check idempotency.
// asynq's other sentinel errors (queue and task inspection, unregistered handlers and the like) are not on the
// enqueue path and are not re-exported.
//
// Both are asynq's errors of the same name themselves (the same value, not a wrapper), so errors.Is treats them as
// equal either way.
var (
	ErrTaskIDConflict = asynq.ErrTaskIDConflict
	ErrDuplicateTask  = asynq.ErrDuplicateTask
)

// TaskInfo holds the result of a successful Enqueue.
type TaskInfo struct {
	ID    string
	Queue string
	State string
}

// EnqueueOption wraps an asynq.Option.
//
// The allowlist covers 9 options; asynq's 10th Option, Group (task aggregation), is rejected on purpose: aggregation
// only works with both halves, tagging on the enqueue side and a GroupAggregator callback on the server side. Keel
// provides no aggregation assembly surface, so adding the Option alone would only make tasks pile up until the grace
// period ends and then run one by one. When aggregation is really needed, add it together with the assembly surface,
// not just this half.
type EnqueueOption struct {
	inner asynq.Option
}

func MaxRetry(n int) EnqueueOption            { return EnqueueOption{inner: asynq.MaxRetry(n)} }
func Queue(name string) EnqueueOption         { return EnqueueOption{inner: asynq.Queue(name)} }
func TaskID(id string) EnqueueOption          { return EnqueueOption{inner: asynq.TaskID(id)} }
func Timeout(d time.Duration) EnqueueOption   { return EnqueueOption{inner: asynq.Timeout(d)} }
func Deadline(t time.Time) EnqueueOption      { return EnqueueOption{inner: asynq.Deadline(t)} }
func Unique(ttl time.Duration) EnqueueOption  { return EnqueueOption{inner: asynq.Unique(ttl)} }
func ProcessIn(d time.Duration) EnqueueOption { return EnqueueOption{inner: asynq.ProcessIn(d)} }
func ProcessAt(t time.Time) EnqueueOption     { return EnqueueOption{inner: asynq.ProcessAt(t)} }
func Retention(d time.Duration) EnqueueOption { return EnqueueOption{inner: asynq.Retention(d)} }

// errZeroOption reports a zero-value EnqueueOption (inner is nil), a programming error that only appears when the
// constructors are bypassed. Left unchecked, the two paths would even behave differently: asynq silently ignores a
// nil option (the type switch in composeOptions matches no case), while cron validation panics on a nil dereference.
// Both now fail on the spot.
var errZeroOption = errors.New("kasynq: zero-value EnqueueOption; use the option constructors (MaxRetry, Queue, ...)")

// validateEnqueueOptions mirrors, ahead of time, the option validation asynq only does at enqueue time
// (composeOptions, client.go:246, exactly three rules: non-empty Queue name, non-empty TaskID, Unique >= 1s). It is
// only used for cron registration: an immediate enqueue gets asynq's error synchronously, on the spot, but a cron
// task is enqueued when it fires, so without the check at registration "the application starts normally and the
// task can never be enqueued".
func validateEnqueueOptions(opts []EnqueueOption) error {
	for _, o := range opts {
		if o.inner == nil {
			return errs.Rules.Wrap(errZeroOption, "kasynq.zero-option", "an EnqueueOption is the zero value EnqueueOption{}", "build every option with its constructor, such as MaxRetry or Queue")
		}
		switch o.inner.Type() {
		case asynq.QueueOpt:
			if q, _ := o.inner.Value().(string); strings.TrimSpace(q) == "" {
				return errs.Rules.New("kasynq.queue-empty", "a cron registration passes Queue with an empty or blank name", "pass a queue name listed under asynq.<name>.queues, or drop the Queue option to use the default queue")
			}
		case asynq.TaskIDOpt:
			if id, _ := o.inner.Value().(string); strings.TrimSpace(id) == "" {
				return errs.Rules.New("kasynq.task-id-empty", "a cron registration passes TaskID with an empty or blank ID", "pass a non-empty ID to TaskID, or drop the option")
			}
		case asynq.UniqueOpt:
			if d, _ := o.inner.Value().(time.Duration); d < time.Second {
				return errs.Rules.New("kasynq.unique-ttl", fmt.Sprintf("a cron registration passes Unique(%v), below asynq's 1s minimum", d), "pass Unique a TTL of at least time.Second")
			}
		}
	}
	return nil
}

func toAsynqOptions(opts []EnqueueOption) []asynq.Option {
	result := make([]asynq.Option, len(opts))
	for i, o := range opts {
		result[i] = o.inner
	}
	return result
}

// TaskClient enqueues tasks. Business code depends on the Enqueuer it satisfies, not on TaskClient.
type TaskClient struct {
	client *asynq.Client
}

// NewTaskClient takes only what it actually uses.
//
// It has no cleanup: the client is built with NewClientFromRedisClient (sharedConnection), this module owns no
// resources, and the redis connection's lifecycle belongs to the provider that injects it.
//
// There is no NewNamedTaskClient: the enqueueing client has nothing to do with the asynq.<name> config section.
// Concurrency and queue priorities are worker-side parameters, and the queue is chosen at Enqueue time with
// Queue(...). To use another redis instance, pass that connection in at assembly.
func NewTaskClient(rdb redis.UniversalClient) *TaskClient {
	return &TaskClient{client: asynq.NewClientFromRedisClient(rdb)}
}

// Client returns the underlying asynq client, so users can call native capabilities outside the practice (Ping,
// enqueueing with native Options and the like; *asynq.Client only has the enqueue surface). Task state inspection does
// not go through it: build an asynq.Inspector separately with NewInspectorFromRedisClient(rdb), where rdb is the
// connection injected at assembly.
//
// **Calls through this escape hatch bypass kasynq's wrapping**: there is no task.enqueue span, and parameters are no
// longer constrained by EnqueueOption. Use Enqueue when you need those.
func (c *TaskClient) Client() *asynq.Client { return c.client }

// Enqueue is documented on the Enqueuer interface.
func (c *TaskClient) Enqueue(ctx context.Context, typeName string, payload []byte, opts ...EnqueueOption) (*TaskInfo, error) {
	// Only zero-value options are rejected here (see errZeroOption); the other option checks are not repeated
	// because asynq does them synchronously in EnqueueContext and returns the error to the caller on the spot.
	for _, o := range opts {
		if o.inner == nil {
			return nil, errs.Rules.Wrap(errZeroOption, "kasynq.zero-option", "an EnqueueOption is the zero value EnqueueOption{}", "build every option with its constructor, such as MaxRetry or Queue")
		}
	}
	// traceparent travels in the task headers and otelTaskMiddleware extracts it on the consuming side to continue
	// the trace, so one asynchronous business operation is one trace. StartSend starts the span before injecting, so
	// the consumer's parent is this producer span. When otel is not enabled the global propagator is a no-op and the
	// headers stay empty, which is harmless. messaging.destination.name is the queue name (an approximation, taken
	// from Queue(...) in opts and falling back to asynq's default queue name "default"): it is the generic "where the
	// message goes" attribute, while the task name (task.type) is a task-specific attribute; they are different
	// things and are still set separately.
	headers := propagation.MapCarrier{}
	ctx, span := msgtrace.StartSend(ctx, "asynq", "task.enqueue", queueNameOf(opts), headers,
		attribute.String("task.type", typeName))
	defer span.End()

	info, err := c.client.EnqueueContext(ctx,
		asynq.NewTaskWithHeaders(typeName, payload, headers),
		toAsynqOptions(opts)...,
	)
	if err != nil {
		msgtrace.MarkFailed(span, err)
		return nil, err
	}
	return &TaskInfo{ID: info.ID, Queue: info.Queue, State: info.State.String()}, nil
}

// queueNameOf returns the value of Queue(...) among the options, falling back to asynq's default queue name
// "default" (hibiken/asynq v0.26.0 internal/base.DefaultQueueName, unexported, so its literal is mirrored). It is
// only used for the span's messaging.destination.name and does not affect actual enqueueing, which asynq decides by
// the same rule in EnqueueContext.
func queueNameOf(opts []EnqueueOption) string {
	for _, o := range opts {
		if o.inner != nil && o.inner.Type() == asynq.QueueOpt {
			if q, ok := o.inner.Value().(string); ok && q != "" {
				return q
			}
		}
	}
	return "default"
}
