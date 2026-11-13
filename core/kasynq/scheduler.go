package kasynq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hibiken/asynq"

	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/core/kconfig"
	"github.com/go-keel/keel/internal/appid"
	"github.com/go-keel/keel/internal/errs"
)

// SchedulerSetup registers cron tasks into a SchedulerServer. It returns no error: Register failures are recorded on
// the SchedulerServer and returned at once by NewSchedulerServer after setup runs, the same shape as the
// registration callbacks for event subscription and task handling.
type SchedulerSetup func(r ScheduleRegistrar)

// ScheduleRegistrar is the registration interface exposed to SchedulerSetup.
type ScheduleRegistrar interface {
	// Register is the bytes-level registration entry point; ordinary business code uses TaskType.RegisterCron
	// (which goes through it). It returns the entry ID and whether this call failed: SchedulerSetup itself returns
	// no error, but the error is still handed to the caller here for cases that need to handle it on the spot.
	// Ignoring it is fine too, because the same error has already been accumulated into NewSchedulerServer's return
	// value.
	Register(cronspec string, typeName string, payload []byte, opts ...EnqueueOption) (string, error)
	// Fail records into the accumulated error a failure already known before Register is called. The typical case
	// is a payload encoding failure in TaskType.RegisterCron: it happens at json.Marshal, Register is never called,
	// and so Register's internal accumulation never sees it. SchedulerSetup itself returns no error, so unless such
	// a failure is reported explicitly it vanishes entirely when the caller ignores the return value as the
	// recommended usage does: the server is built successfully but the task is never registered. It is a no-op when
	// err is nil.
	Fail(err error)
}

// SchedulerServer is an asynq-backed cron scheduler that implements transport.Server. Any number of replicas may run:
// only the one holding the application's lease in Redis enqueues, the others wait to take over.
type SchedulerServer struct {
	rdb       redis.UniversalClient
	scheduler *asynq.Scheduler
	lease     lease

	mu       sync.Mutex
	stop     chan struct{} // closed by Stop; ends waiting for or holding the lease
	stopping bool
	running  bool // the asynq scheduler has started and not shut down
	// err accumulates Register's registration errors (empty task name, invalid options, bad cronspec);
	// NewSchedulerServer returns them at once after setup runs.
	err error
}

func schedulerOpts() *asynq.SchedulerOpts {
	return &asynq.SchedulerOpts{
		// UTC is set explicitly: asynq's default happens to be UTC too, but here it is a convention, not a
		// coincidence, and must not rely on the upstream default staying the same.
		Location: time.UTC,
		Logger:   schedulerLogger{},
		// EnqueueErrorHandler: by default asynq silently swallows enqueue failures of cron triggers, so they are
		// logged here (otherwise a scheduled task that "did not run" could not be diagnosed).
		//
		// Deprecated upstream in favor of PostEnqueueFunc, but PostEnqueueFunc gets a nil *TaskInfo on failure
		// (asynq v0.26.0 scheduler.go enqueueJob.Run), so the log would lose the task type. Keep this until
		// asynq removes it or PostEnqueueFunc carries the task.
		EnqueueErrorHandler: func(task *asynq.Task, opts []asynq.Option, err error) { //nolint:staticcheck // see above
			slog.Error("scheduler enqueue failed", "task_type", task.Type(), "error", err)
		},
	}
}

// NewSchedulerServer builds the cron scheduler. It does not read the asynq.<name> config section (concurrency and queue
// priorities are both worker-side parameters); config gives the application's name and instance ID for the lease.
//
// **One replica schedules.** Start blocks until this instance holds the Redis lease <app.domain>:<app.name>:cron_scheduler
// (30s TTL, renewed every 10s), then schedules; other replicas wait in Start and take over within 10s of a clean Stop,
// or within 40s of a crash. Once 20s pass without a successful renewal, or another instance holds the lease, scheduling
// stops and Start returns kasynq.scheduler-lease-lost, so the process exits and its restart waits for the lease again.
// A trigger falling in a handover gap is skipped, not enqueued late.
//
// **Every cronspec is interpreted in UTC**, and the framework offers no time zone setting: the time zone is the
// deployment setting most easily misconfigured across environments, and a misconfiguration shows up as scheduled
// tasks running at the wrong time, with no error. Everything uses the single UTC reference, and the conversion is
// the responsibility of whoever writes the cronspec.
func NewSchedulerServer(config *kconfig.Config, rdb redis.UniversalClient, setup SchedulerSetup) (*SchedulerServer, error) {
	scheduler := asynq.NewSchedulerFromRedisClient(rdb, schedulerOpts())
	domain := config.App.GetDomain()
	if domain == "" {
		domain = "keel"
	}
	s := &SchedulerServer{
		rdb:       rdb,
		scheduler: scheduler,
		lease: lease{
			rdb:   rdb,
			key:   fmt.Sprintf("%s:%s:cron_scheduler", domain, config.App.GetName()),
			owner: appid.LeaseOwner(config.App),
		},
		stop: make(chan struct{}),
	}
	if setup != nil {
		setup(s)
	}
	if s.err != nil {
		return nil, s.err
	}
	return s, nil
}

// Scheduler returns the underlying asynq Scheduler, so users can call native capabilities kasynq does not wrap
// (Unregister, enqueueing customised with asynq.Option, and so on).
//
// **The cronspec time zone is still the UTC fixed at construction**: it is part of SchedulerOpts and does not change
// through this escape hatch.
//
// Do not Start or Shutdown it yourself: SchedulerServer starts it only while holding the lease.
func (s *SchedulerServer) Scheduler() *asynq.Scheduler { return s.scheduler }

// Start checks the redis eviction policy, waits for the lease, then schedules until Stop or until the lease is lost.
func (s *SchedulerServer) Start(ctx context.Context) error {
	if err := checkEvictionPolicy(ctx, s.rdb); err != nil {
		return err
	}
	tick := time.NewTicker(leaseEvery)
	defer tick.Stop()
	for {
		ok, err := s.lease.acquire(ctx)
		if err != nil {
			slog.Error("cron scheduler could not try the lease; retrying in 10s", "key", s.lease.key, "error", err)
		}
		if ok {
			break
		}
		select {
		case <-s.stop:
			return nil
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}

	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		s.lease.release()
		return nil
	}
	if err := s.scheduler.Start(); err != nil {
		s.mu.Unlock()
		s.lease.release()
		return err
	}
	s.running = true
	s.mu.Unlock()
	slog.Info("cron scheduler holds the lease; this instance enqueues cron tasks", "key", s.lease.key, "instance_id", s.lease.owner)

	lastRenew := time.Now()
	for {
		select {
		case <-s.stop:
			return nil
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		ok, err := s.lease.renew(ctx)
		switch {
		case err == nil && ok:
			lastRenew = time.Now()
			continue
		case err != nil && time.Since(lastRenew) < leaseStopAfter:
			slog.Error("cron scheduler lease renewal failed; scheduling stops after 20s without renewal", "key", s.lease.key, "error", err)
			continue
		}
		s.mu.Lock()
		running := s.running
		s.running = false
		s.mu.Unlock()
		if running {
			s.scheduler.Shutdown()
		}
		return errs.Rules.New("kasynq.scheduler-lease-lost",
			fmt.Sprintf("cron scheduler lost its lease %s (last renewed %s ago, renew error: %v), so it stopped scheduling to avoid enqueuing twice", s.lease.key, time.Since(lastRenew).Round(time.Second), err),
			"check that Redis is reachable and that only replicas of this application use the same app.domain and app.name; the restarted process waits for the lease again")
	}
}

// Stop ends Start, shuts the asynq scheduler down if it is running and releases the lease. asynq's Shutdown takes no
// ctx itself, so the ctx budget (app.stop_timeout) bounds the wait, as in WorkerServer.Stop; on timeout the lease is
// kept and expires within 30s, since the scheduler may still enqueue.
func (s *SchedulerServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.stopping {
		s.stopping = true
		close(s.stop)
	}
	running := s.running
	s.running = false
	s.mu.Unlock()
	if running {
		done := make(chan struct{})
		go func() { s.scheduler.Shutdown(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.lease.release()
	return nil
}

// Register is documented on the ScheduleRegistrar interface.
func (s *SchedulerServer) Register(cronspec string, typeName string, payload []byte, opts ...EnqueueOption) (string, error) {
	// An empty task name must be rejected here: asynq's Scheduler.Register only validates the cronspec, and an empty
	// name only errors each time the trigger enqueues, so the application starts normally and the scheduled task
	// never runs. (Immediate enqueueing needs no check: asynq's client errors synchronously in Enqueue, and the
	// caller gets the error on the spot.)
	if strings.TrimSpace(typeName) == "" {
		err := errs.Rules.New("kasynq.task-type-empty", fmt.Sprintf("cron %q is registered with an empty or blank task type name", cronspec), "pass the task type name the worker handles, or use TaskType.RegisterCron")
		s.err = errors.Join(s.err, err)
		return "", err
	}
	// Option validation moves earlier for the same reason: asynq's Scheduler.Register does not compose options, so
	// things like Queue(" ") or Unique(500ms) only error when the trigger enqueues.
	if err := validateEnqueueOptions(opts); err != nil {
		s.err = errors.Join(s.err, err)
		return "", err
	}
	id, err := s.scheduler.Register(
		cronspec,
		asynq.NewTask(typeName, payload),
		toAsynqOptions(opts)...,
	)
	if err != nil {
		// A bad cronspec and the like: SchedulerSetup returns no error, so this failure reaches NewSchedulerServer's
		// single return only by being accumulated here.
		s.err = errors.Join(s.err, err)
		return "", err
	}
	return id, nil
}

// Fail is documented on the ScheduleRegistrar interface.
func (s *SchedulerServer) Fail(err error) {
	if err == nil {
		return
	}
	s.err = errors.Join(s.err, err)
}
