# Tasks (asynq)

Read this before defining, enqueuing, scheduling or consuming a task, or before deciding between a task and an event.

`core/kasynq` provides asynchronous tasks, task queues and cron scheduling on [asynq](https://github.com/hibiken/asynq)
(backed by Redis). Its worker sits at the same layer as the `core/knats` consumers.

## Task or event

Choose by capability, not by weight. kasynq has queue priority, task deduplication, delayed execution, cron and task
status queries; knats only broadcasts real-time events one-to-many and has no delayed delivery. Work with a known
executor is a task (kasynq); something broadcast to unknown subscribers is an event (knats, see
[Messaging](messaging.md)). Both can coexist; do not split one piece of work between them.

- **To do something later, use `ProcessIn` / `ProcessAt`**, not the event side: NATS has no delay mechanism at the
  right cost (reasons in [Messaging](messaging.md)). "Check in 30 minutes whether the note was shared" has a known
  executor and a known time.
- **Multi-step work** with compensation, spanning days, with "which step are we at" queries, is a workflow engine's
  job (Temporal and the like, outside Keel's stack); do not assemble it from cron plus retries.
- **Slow work triggered by an event comes here too.** A knats handler has a processing deadline (10s by default); a
  handler still running at the server's ack timeout (3 times that, 30s by default) gets its message redelivered and
  processed twice. Receive the event, enqueue a task, ack, and do the slow work in
  the task; the code is in [Messaging](messaging.md), "Slow work goes to a task".

## Define a task

Bind the task name and payload type in one place as a **task contract**. Enqueuing, handling and cron all start from
it, so a wrong name or payload fails to compile and JSON encoding and decoding on both sides is done by kasynq:

```go
type IndexNotePayload struct {
    NoteID string `json:"note_id"`
}

// Package-level variable; producer and handler reference the same definition.
var IndexNote = kasynq.NewTaskType[IndexNotePayload]("note:index")
```

Name tasks `<domain>:<action>`, lowercase with underscores.

## Enqueue

```go
info, err := IndexNote.Enqueue(ctx, taskClient, IndexNotePayload{NoteID: id},
    kasynq.MaxRetry(3),
    kasynq.Queue("critical"),
)
```

`Enqueue` opens an OpenTelemetry producer span and injects the trace context into the task headers; the worker
continues it with a consumer span, so one asynchronous operation is one trace, as with knats. A task fired by cron
has no upstream `ctx` and starts a new trace.

The business layer depends on the `kasynq.Enqueuer` interface, not a concrete type; the `TaskClient` from
`NewTaskClient` satisfies it.

### Enqueue options

Every option takes full effect on the producer side:

| Option | Meaning |
|---|---|
| `Queue(name)` | target queue, weighted by the queue priorities in the config |
| `MaxRetry(n)` | retries before the task is archived (default 25) |
| `Timeout(d)` / `Deadline(t)` | time limit of one run, relative or absolute (30 minutes when neither is set) |
| `ProcessIn(d)` / `ProcessAt(t)` | delayed execution, relative or absolute |
| `Unique(ttl)` | deduplicate by content hash: an identical task within `ttl` is not enqueued again |
| `TaskID(id)` | caller-chosen deduplication key: enqueuing the same ID before the first finishes conflicts |
| `Retention(d)` | keep a completed task for `d` for status queries; without it a completed task is deleted |

A `TaskID` collision returns `kasynq.ErrTaskIDConflict` and a `Unique` collision `kasynq.ErrDuplicateTask`. Both are
the usual way to make enqueuing idempotent: on `errors.Is`, treat the task as already enqueued, without importing
asynq.

asynq's `Group` option (task aggregation) is deliberately not offered. Aggregation needs a mark on the producer side
and an aggregation callback on the server side; Keel has no assembly point for the second half, so the option alone
would only make tasks wait for a timeout and then run one by one.

## Handle

The worker's setup function returns a `kasynq.WorkerSetup` and registers typed handlers through the task contracts:

```go
func NewTaskServer(indexer *service.Indexer) kasynq.WorkerSetup {
    return func(r kasynq.HandlerRegistrar) {
        biz.IndexNote.Handle(r, indexer.IndexNote)
    }
}

func (s *Indexer) IndexNote(ctx context.Context, p biz.IndexNotePayload) error {
    // An error retries per the retry policy; when retries run out the task is archived.
    return s.uc.Index(ctx, p.NoteID)
}
```

**A payload that does not decode is not retried**: it never will decode, so `Handle` marks the error `SkipRetry` and
the task goes straight to the archive without using up retries.

Registering a task name twice, or an empty name, is a startup error. asynq's `ServeMux.Handle` panics on it; kasynq
recovers that into an error, records it, and `NewWorkerServer` / `NewNamedWorkerServer` return it after the setup has
run. Event consumers and cron registrations follow the same rule: the registration callback returns no error, the
constructor returns them all once.

## Cron

The same task contract can be scheduled, so the handler is written once. **Cron specs are always UTC**; there is no
time zone setting, and converting is the job of whoever writes the spec:

```go
var DailyDigest = kasynq.NewTaskType[struct{}]("note:daily_digest")

func NewSchedulerServer() kasynq.SchedulerSetup {
    return func(r kasynq.ScheduleRegistrar) {
        // 00:00 UTC. The error may be dropped here: an invalid spec, option or payload is recorded on the
        // SchedulerServer and returned by NewSchedulerServer.
        _, _ = biz.DailyDigest.RegisterCron(r, "0 0 * * *", struct{}{})
    }
}
```

The returned entry ID is only needed to `Unregister` dynamically through the raw `Scheduler()` handle; a registration
at startup drops it.

**Every cron task needs a handler on the worker.** The scheduler and the worker are separate servers that do not know
each other, so registration cannot check it: a cron task with no `Handle` starts fine, is enqueued on schedule, and
fails in the worker until it is archived.

### One replica schedules

asynq's scheduler has no leader election: N replicas would enqueue every trigger N times. So a `SchedulerServer` first
takes the Redis lease `<app.domain>:<app.name>:cron_scheduler` (30s TTL, renewed every 10s) and schedules only while it
holds it; other replicas wait in `Start` and are a hot standby. Deploy as many replicas as you like.

- A clean `Stop` releases the lease and a standby takes over within 10s; after a crash, within 40s. A trigger that falls
  in the gap is skipped, not enqueued late.
- After 20s without a successful renewal, or when another instance holds the lease, scheduling stops and `Start`
  returns `kasynq.scheduler-lease-lost`: the process exits, and its restart waits for the lease again. Stopping before
  the 30s TTL ends means two replicas never schedule at once.
- The lease is per application name, so applications sharing one Redis do not block each other; two different
  applications with the same `app.domain` and `app.name` would.

## Byte level

For a custom encoding (protobuf, an encrypted payload) or a task name chosen at run time, skip the task contract;
the payload goes in and out unchanged:

```go
info, err := taskClient.Enqueue(ctx, "note:custom", payloadBytes, kasynq.Queue("low"))

r.HandleFunc("note:custom", func(ctx context.Context, payload []byte) error { ... })

entryID, err := r.Register("@every 1h", "note:custom", payloadBytes)
```

The byte level is also where a test double plugs in: implement `kasynq.Enqueuer`'s single `Enqueue` method, with no
asynq import, and typed `TaskType.Enqueue` calls run against it; the test sees the task name and the encoded payload.

## Assembly

Build Redis first, then the servers, then pass them to `factory.New` (general rules in [Assembly](assembly.md)):

```go
rdb, redisCleanup, err := kredis.NewRedis(cfg)
taskClient := kasynq.NewTaskClient(rdb) // no cleanup: the Redis cleanup above owns the connection

workerServer, err := kasynq.NewWorkerServer(cfg, rdb, server.NewTaskServer(indexer))
schedulerServer, err := kasynq.NewSchedulerServer(cfg, rdb, server.NewSchedulerServer())

app, err := factory.New(workerServer, schedulerServer)
```

`WorkerServer` and `SchedulerServer` both implement `keel.Server`. Several worker pools with different config
sections use `NewNamedWorkerServer(cfg, rdb, "reports", setup)`.

## Configuration

```yaml
asynq:
  default:
    concurrency: 10
    queues:
      critical: 6
      default: 3
      low: 1
    strict_priority: false
```

This section only covers the worker: concurrency and queue priorities. Which Redis asynq uses is not configured here;
it is injected like every other dependency, chosen in `di.go`:

```go
rdb, redisCleanup, err := kredis.NewNamedRedis(cfg, "queues")
taskClient := kasynq.NewTaskClient(rdb)
```

**The section must exist**: a worker with no `asynq.<name>` section fails to start; the minimal config is
`asynq: {default: {}}`. kasynq does not fall back to asynq's defaults, because config parsing drops unknown keys, so a
misspelt section disappears entirely, and a worker on defaults listens only to the `default` queue: tasks enqueued to
`critical` pile up silently while the service looks healthy. For the same reason a blank queue name or a non-positive
weight fails config loading, where asynq would silently filter it out.

### Shutdown

How long a stop waits comes only from `app.stop_timeout` (see [Configuration](config.md)); the `asynq` section has no
timeout of its own.

asynq also has an internal limit on how long a running task may continue before it is pushed back to the queue
(`asynq.Config.ShutdownTimeout`), and asynq reads its zero value as a hard-coded 8 seconds, not "no limit".
kasynq sets it from `app.stop_timeout`, minus 2 seconds for asynq's own wrap-up, so a 12-second task is not cut off
at 8 seconds when 30 are configured. This gives no absolute guarantee: `asynq.Server.Shutdown()` returns nothing, so
when a task is pushed back for running over budget `Stop` cannot know, and `nil` only means asynq considers itself
done.

## Redis eviction policy

asynq keeps its queues in Redis, so a Redis that may evict any key would lose tasks without an error. The worker and
the scheduler read `maxmemory_policy` with `INFO memory` when they start (managed services often disable
`CONFIG GET`) and refuse to start on an `allkeys-*` policy, or when the field is missing. `noeviction` and
`volatile-*` pass; open-source Redis defaults to `noeviction`, and most managed Redis services default to
`volatile-lru`. With `volatile-*`, every other key in that Redis (cache entries from `kredis`) must carry a TTL,
because only keys with a TTL are evicted and keys without one fill memory until writes fail. There is no switch to
skip this check.

## Raw asynq

For what the wrapper does not cover, use the handles: `TaskClient.Client()` (`Ping`, enqueuing with native options;
`*asynq.Client` only enqueues), `WorkerServer.Mux()` / `Server()`, and `SchedulerServer.Scheduler()` (dynamic
`Unregister`).

Task status queries do not go through `Client()`: create an `asynq.Inspector` with
`asynq.NewInspectorFromRedisClient(rdb)`, passing the Redis connection injected at assembly, so keep `rdb` reachable
in `di.go`.

A handler registered with the native signature through `Mux()` is still covered by kasynq's tracing and panic
recovery middleware, because asynq applies middleware at dispatch; the differences are the raw `*asynq.Task` argument
and that asynq itself reports duplicate registration. `Client()` bypasses the wrapper entirely: no enqueue span, and
options are not validated.
