// In-package: uses the lease helpers in lease_internal_test.go.

package kasynq

import (
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// SchedulerSetup runs synchronously at construction, so an invalid cronspec must error at Register, not be
// discovered after the scheduler starts and the scheduled task turns out not to be registered.
func TestSchedulerRegister_BadCronspec(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	s, err := NewSchedulerServer(schedulerTestConfig(), rdb, nil)
	if err != nil {
		t.Fatalf("new scheduler: %v", err)
	}
	if _, err := s.Register("not-a-cronspec", "t", nil); err == nil {
		t.Fatal("invalid cronspec should return error")
	}
	// An empty task name must likewise be rejected at startup: asynq only errors when the trigger enqueues, so the
	// application starts normally and the scheduled task never runs, which is subtler than a mistyped cronspec.
	if _, err := s.Register("@every 1m", "", nil); err == nil {
		t.Fatal("empty task type name should return error at registration")
	}
}

// Invalid EnqueueOptions belong with the empty task name: asynq's Scheduler.Register does not compose options, so
// things like Queue(" ") only error each time the trigger enqueues, and they must be rejected at registration.
func TestSchedulerRegister_InvalidOptions(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	s, err := NewSchedulerServer(schedulerTestConfig(), rdb, nil)
	if err != nil {
		t.Fatalf("new scheduler: %v", err)
	}
	for name, opt := range map[string]EnqueueOption{
		"blank queue":   Queue(" "),
		"empty task id": TaskID(""),
		"short unique":  Unique(500 * time.Millisecond),
		"zero value":    {},
	} {
		if _, err := s.Register("@every 1m", "t", nil, opt); err == nil {
			t.Errorf("%s: should be rejected at registration, asynq only errors at trigger time", name)
		}
	}
	// valid options register as usual
	if _, err := s.Register("@every 1m", "t", nil,
		Queue("critical"), TaskID("x"), Unique(time.Hour), MaxRetry(3)); err != nil {
		t.Errorf("valid options should register: %v", err)
	}
}

// SchedulerSetup returns no error: Register failures are recorded on the SchedulerServer and returned at once by
// NewSchedulerServer after setup runs, so even when setup ignores Register's return value (the common way to call
// RegisterCron is to drop it; see docs/tasks.md), the error does not vanish.
func TestNewSchedulerServer_SetupErrorsAccumulateWithoutReturn(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_, err := NewSchedulerServer(schedulerTestConfig(), rdb, func(r ScheduleRegistrar) {
		_, _ = r.Register("not-a-cronspec", "t", nil) // return value deliberately dropped
	})
	if err == nil {
		t.Fatal("a bad cronspec should make NewSchedulerServer return an error even when setup ignores it")
	}
}
