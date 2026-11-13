package kasynq_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-keel/keel/core/kasynq"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

type demoPayload struct {
	ID string `json:"id"`
}

var demoTask = kasynq.NewTaskType[demoPayload]("test:demo")

// fakeClient implements only Enqueuer, which is how users fake it in biz tests, without importing asynq or
// implementing Client() along the way. It also proves the strongly typed TaskType.Enqueue runs directly against it:
// if TaskType.Enqueue went back to taking a TaskClient, this would fail to compile.
type fakeClient struct {
	typeName string
	payload  []byte
	opts     int
}

func (f *fakeClient) Enqueue(_ context.Context, typeName string, payload []byte, opts ...kasynq.EnqueueOption) (*kasynq.TaskInfo, error) {
	f.typeName, f.payload, f.opts = typeName, payload, len(opts)
	return &kasynq.TaskInfo{ID: "1"}, nil
}

// Business code relies on errors.Is(err, kasynq.ErrTaskIDConflict) for idempotency: a collision means the task was
// already enqueued. Both values must match the errors asynq actually returns from Enqueue, or the idempotency branch
// never hits and a duplicate enqueue is retried as a failure. The test collides for real against an in-process
// miniredis instead of proving it with hand-built error values.
func TestEnqueue_ConflictErrorsMatchExported(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  kasynq.EnqueueOption
		want error
	}{
		{"TaskID", kasynq.TaskID("same"), kasynq.ErrTaskIDConflict},
		{"Unique", kasynq.Unique(time.Minute), kasynq.ErrDuplicateTask},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			c := kasynq.NewTaskClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
			ctx := context.Background()
			if _, err := c.Enqueue(ctx, "t", []byte("p"), tc.opt); err != nil {
				t.Fatalf("first enqueue: %v", err)
			}
			_, err := c.Enqueue(ctx, "t", []byte("p"), tc.opt)
			if !errors.Is(err, tc.want) {
				t.Fatalf("second enqueue: got %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

// TaskType.Enqueue's glue: the task name comes from the definition, the payload is JSON-encoded and Options pass
// through.
func TestTaskType_EnqueueGlue(t *testing.T) {
	fc := &fakeClient{}
	info, err := demoTask.Enqueue(context.Background(), fc, demoPayload{ID: "42"}, kasynq.MaxRetry(3), kasynq.Queue("low"))
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "1" {
		t.Errorf("info = %+v", info)
	}
	if fc.typeName != "test:demo" || string(fc.payload) != `{"id":"42"}` || fc.opts != 2 {
		t.Errorf("glue mismatch: %q %q %d", fc.typeName, fc.payload, fc.opts)
	}
}

type fakeRegistrar struct {
	fns map[string]func(ctx context.Context, payload []byte) error
}

func (f *fakeRegistrar) HandleFunc(taskType string, fn func(ctx context.Context, payload []byte) error) {
	f.fns[taskType] = fn
}

// A bad payload can never be decoded and retrying it only spins: the decode-failure error must carry SkipRetry so
// asynq archives it directly instead of burning through MaxRetry attempts.
func TestTaskType_HandleDecodeFailureSkipsRetry(t *testing.T) {
	r := &fakeRegistrar{fns: map[string]func(context.Context, []byte) error{}}
	var got demoPayload
	demoTask.Handle(r, func(_ context.Context, p demoPayload) error {
		got = p
		return nil
	})

	fn := r.fns["test:demo"]
	if fn == nil {
		t.Fatal("handler not registered under task name")
	}
	if err := fn(context.Background(), []byte(`{"id":"42"}`)); err != nil || got.ID != "42" {
		t.Fatalf("decode ok path: err=%v got=%+v", err, got)
	}
	err := fn(context.Background(), []byte(`{broken`))
	if err == nil {
		t.Fatal("bad payload should error")
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("decode failure must carry SkipRetry, got: %v", err)
	}
}
