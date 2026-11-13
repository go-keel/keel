# Messaging (NATS JetStream)

Read this before publishing or consuming events, or before deciding between an event and a task.

`core/knats` publishes and consumes real-time events on NATS JetStream. It needs NATS Server 2.14 or
later, the version it is tested against; servers before 2.10.4 ignore `TermWithReason`, so giving up on a message
would not work there.

## Model

A stream collects one kind of event; any number of durables consume the same messages independently:

```
                         ┌── durable: notify   sends notifications
publish("note.created") ─┤
  published once, the    └── durable: audit    writes the audit log
  publisher knows no subscriber
```

The publisher does not know its subscribers, so adding a subscriber needs no change on the publishing side.

Two things behave differently. A **stream is an append-only log**: a message stays there whoever has read it, until
retention removes it. A **durable is the queue**: the cursor, the set of unacknowledged messages and the redelivery
count all live on the durable. One stream with N cursors is how one event reaches N consumers.

What knats does for you:

- Publishing waits for the server's ack, so you do not track delivery confirmation yourself.
- The `nats.go` client reconnects after a disconnect, retrying without limit. At startup it does not wait: when no
  server answers, `NewClient` returns the error and the process does not start.
- Creating a publisher or starting a consumer checks the stream: it is created when missing, adopted as is when it
  exists, and rejected when it does not cover this stream's subjects (see "Who creates a stream decides its config").
- A failed message is redelivered on a backoff table, up to `MaxDeliver` deliveries; a given-up message is recorded
  as a dead letter and can be replayed.
- The OpenTelemetry trace continues across the hop: the trace context travels in the message header.
- A handler panic is recovered and treated as a retry.

## Configuration

```yaml
nats:
  default:
    url: "nats://nats:4222"
    creds_file: ""                # nkey/JWT credentials file; empty means no authentication
    stream_max_age: 604800s       # retention of streams knats creates; default 7 days
    stream_replicas: 3            # default 1; in a cluster set it to match the cluster
    dead_letter_max_age: 2592000s # retention of the DEAD_LETTER stream; default 30 days
```

`stream_max_age` decides how long an event can be looked up, and how long a dead letter's original message can still
be replayed. `stream_replicas` set too low loses data when a broker fails. All three only apply to streams knats
creates itself.

Zero or absent means the default; a negative value fails config loading, because a setting that is written but has
no effect is the hardest kind of bug to find.

## Naming

Publisher and consumer meet through these rules (here `app.domain` is `acme`, the stream is `notes`):

| Concept | Rule | Example |
|---|---|---|
| NATS stream name | `<domain>_<stream>` | `acme_notes` |
| subject | `<domain>.<stream>.<topic>` | `acme.notes.note.created` |

The stream name uses an underscore because NATS forbids `.` in stream names while subjects are dot-separated. A
stream name containing a dot is rejected when the publisher is created or the consumer registered
(`ErrStreamNameDotted`), before any request reaches the server.

knats creates no stream of its own under your domain, so there are no reserved stream names. (`DEAD_LETTER` is one
stream per NATS account, outside the domain prefix.)

Write only the topic part. A subject that repeats the prefix (`acme.notes.`) expands to
`acme.notes.acme.notes.note.created`: valid syntax that publishes and subscribes fine, and never matches, without a
single log line. Only knats can tell, so registration and publishing both reject it with `ErrSubjectPrefixed`.

## Event contracts

Declare the stream once as a **stream contract** (`knats.NewStream`), and bind each event's stream, subject and
payload type in one place as an **event contract** (`knats.NewEventType`). Publishing and consuming both start from
these definitions, so the stream name is written once in the whole repository instead of as scattered strings that can
silently diverge. A misspelt variable fails to compile; referencing the wrong existing contract does not, so name them
clearly. The shape matches the task contract `kasynq.TaskType` (see [Tasks](tasks.md)).

```go
type NoteCreated struct {
    ID int64 `json:"id"`
}

// Package-level variables; publisher and consumer reference the same definitions.
var NotesStream = knats.NewStream("notes")
var NoteCreatedEvent = knats.NewEventType[NoteCreated](NotesStream, "note.created")
```

Publish through a publisher created with every stream this process publishes to:

```go
publisher, err := knats.NewPublisher(client, NotesStream) // checks each stream; an error stops startup
err = NoteCreatedEvent.Publish(ctx, publisher, NoteCreated{ID: 42})
```

The publisher is the only way to publish; `Client` itself cannot. Publishing to a stream not passed to
`NewPublisher` returns `ErrStreamNotDeclared`; passing no stream returns `ErrNoStreams`. The check at creation covers
stream lookup/creation permission and subject coverage, not permission to publish on the subject, which can still fail
at run time.

Consume by registering inside the `ConsumerSetup` (see "Assembly"):

```go
NoteCreatedEvent.Handle(r, func(ctx context.Context, ev NoteCreated, d knats.DeliveryInfo) knats.HandlerResult {
    if err := notify(ctx, ev.ID); err != nil {
        return knats.RetryLater(err)
    }
    return knats.OK()
}, knats.EventOptions{Durable: "notify"}) // Durable is unique within the stream
```

The handler receives the decoded payload and a read-only `DeliveryInfo` (`RetryAttempts`, `Sequence`). A payload that
does not decode never reaches the handler: knats terminates it and logs an ERROR. A bad payload can only come from a
byte-level publish that bypassed the contract, and retrying will not decode it.

`EventOptions` has no `Stream` or `Subject`: the contract gives them, and a field that would be silently overridden is
not offered.

## Byte-level publish and consume

Use the byte level for a custom encoding, a subject built at run time, or a wildcard subscription where one handler
takes several topics. The destination is still a stream contract passed to `NewPublisher`; the subject is a run-time
string. To publish to a stream you do not know at startup, use the raw handle (see "Raw NATS").

```go
err = publisher.Publish(ctx, NotesStream, "note.created", &NoteCreated{ID: 42})

// custom header or a non-JSON body
err = publisher.PublishEx(ctx, NotesStream, "note.created", knats.Publishing{
    ContentType: "application/json",
    Body:        body,
    Header:      map[string]string{"x-tenant": "acme"},
})
```

A test double for publishing implements `knats.Publisher` (`Publish` and `PublishEx`); `EventType.Publish` accepts
it.

All consumers are registered once, in the setup passed to `NewConsumerServer`; `Client` has no registration method:

```go
consumerServer, err := knats.NewConsumerServer(client, func(r knats.ConsumerRegistrar) {
    r.Register(func(ctx context.Context, d *knats.Delivery) knats.HandlerResult {
        switch d.Topic { // the topic without the domain/stream prefix
        case "note.created": ...
        case "note.archived": ...
        }
        return knats.OK()
    }, knats.ConsumerOptions{
        Stream:  NotesStream,
        Subject: "note.>", // empty subscribes to the whole stream
        Durable: "audit",
    })
})
```

`Register` returns no error. A duplicate durable, an invalid subject, a negative option or a nil handler is recorded
on the registrar and returned once by `NewConsumerServer` after setup runs. kasynq's handler and cron registration
work the same way: no panic, no silence, one exit for errors. No registered consumer returns `ErrNoConsumers`.

`d.AsJson(&v)` decodes a byte-level body; when it fails, return `knats.Failed(err)`, since retrying will not help.

### Filtering happens on the server

`Subject` uses NATS subject matching and becomes the durable's `FilterSubject`, so non-matching messages are never
delivered:

| Subject | Matches |
|---|---|
| `"note.created"` | exactly that topic |
| `"note.created.*"` | `*` is exactly one token: `note.created.a` yes, `note.created.a.b` no |
| `"note.>"` | `>` is one or more tokens, only at the end |
| `""` | the whole stream |

Put every filter you can into the subject. Filtering on payload content can only happen in the handler, after the
message was delivered and took bandwidth and ack quota. **A message the handler skips returns `knats.OK()`**: OK
is the ack, meaning "done with this one", not "succeeded". `knats.RetryLater` would bring it back on the
backoff table until it is given up.

## Handler results, retries and dead letters

| Result | Effect |
|---|---|
| `knats.OK()` | ack; the message is done |
| `knats.RetryLater(err)` | redelivered after 1s, 3s, 10s, 30s, then 60s (capped), until `MaxDeliver` |
| `knats.Failed(err)` | terminated now, logged at ERROR with the durable, recorded as a dead letter |
| handler panic | logged with the stack and treated as `RetryLater`: a panic is not necessarily deterministic |

The error given to `RetryLater` and `Failed` becomes the give-up reason in the ERROR log and the dead letter, so an
operator replaying it sees why without searching the logs; put what the operator needs in it, not personal data. A
contract payload that does not decode is given up with the decode error as the reason, without calling the handler.

`MaxDeliver` (on `ConsumerOptions` and `EventOptions`) caps deliveries of one message, the first included. The default
is 4: the first delivery plus 3 retries. When the last delivery still asks to retry (or panics), the message is given
up with a reason instead of redelivered. `MaxDeliver` is written to the server-side consumer, so every instance of a
durable must set the same value.

### Dead letters: advisories, not copies

When knats gives up on a message it terminates it with a reason. The server publishes a terminated (or
max-deliveries) advisory carrying the stream, consumer, stream sequence, delivery count and reason, and the
`DEAD_LETTER` stream keeps those advisories. The first consumer server that starts creates it, because advisories
published before it exists are lost; its retention is `nats.<name>.dead_letter_max_age` (default 30 days).

The message itself is not copied: it stays in its own stream until that stream's retention (`stream_max_age`,
default 7 days) ends. `knats.ListDeadLetters` and `knats.ReplayDeadLetter`, and the command

```bash
go run github.com/go-keel/keel/core/knats/cmd/knats-deadletter -url nats://host:4222 list [-consumer NAME]
go run github.com/go-keel/keel/core/knats/cmd/knats-deadletter -url nats://host:4222 replay SEQ...
```

read the advisories and republish the original to its subject. Fix the cause before replaying. A replay reaches every
consumer of that subject, not only the one that gave up; handlers are idempotent anyway (at-least-once delivery).
Replaying after the original's retention returns `ErrOriginalGone`.

### Delivery

A byte-level handler gets `*knats.Delivery`; an event-contract handler gets the decoded payload plus `DeliveryInfo`
(`RetryAttempts` and `Sequence` only).

- `Topic`: the topic without the prefix, as passed when publishing.
- `Body` and `Header`: the message body and headers.
- `RetryAttempts`: redeliveries so far, 0 on the first delivery.
- `Sequence`: the message's sequence in the stream. It increases monotonically and does not change on redelivery, so
  it serves as a deduplication key.

The raw `jetstream.Msg` is not exposed: knats does the ack, nak and term, and the one useful fact on it, the
sequence, is already in `Delivery`.

## Processing time and concurrency

Two knobs, both with defaults: `MaxProcessTime` bounds how long one message may take, `Workers` how many run at once.

### `MaxProcessTime` is the handler's deadline

`MaxProcessTime` (default 10s) sets a deadline on the handler's `ctx`; when it passes, `ctx.Done()` closes and
downstream calls fail. knats cannot interrupt a handler (Go has no way to), so a handler that ignores `ctx` and still
returns within the server's `AckWait` is acked normally.

knats sets `AckWait` to `3 × MaxProcessTime` (30s by default). One share is the handler's time, one is the wait of a
prefetched message behind the one in progress, one is margin for jitter and the ack itself. Keep a single message
within `MaxProcessTime` and the rest is accounted for. If work needs longer, turn it into a task (see "Slow work goes
to a task") rather than raising the deadline.

### The prefetch window follows `Workers`

`Workers` (default 1) is the number of goroutines in one instance, and also that instance's prefetch window: knats
passes `Workers` as `PullMaxMessages` instead of the upstream default of 500. A message prefetched into the client
already counts as delivered on the server and its `AckWait` is running. With a window of 500, `Workers=1` and one
second per message, everything from about the 31st message on times out before reaching the handler: the same message
is processed twice and a redelivery storm follows.
The server also has a durable-wide `MaxAckPending` quota shared by all instances. knats leaves it at 0, so operators
control it in layers: the stream's `ConsumerLimits`, then server or account limits, then the server default of 1000.
Setting it in code would override the stream's `ConsumerLimits`, and a value above the server limit makes consumer
creation fail instead of being narrowed.

### Several instances

Running several instances of one durable is ordinary horizontal scaling; messages are shared between them.

- **`MaxProcessTime` and `MaxDeliver` must be equal on every instance.** They are written to the server-side consumer
  on every start, so the instance that started last wins: with 10s on one and 60s on another, the second one's
  handlers are redelivered before their own deadline. `Workers` may differ; it only affects each client's prefetch.
- **Stay below the server quota**: K instances each prefetch `Workers` messages. The default of 1000 is far from 8
  instances × 8 workers; at larger scale configure stream or account limits on the NATS side.

## Duplicates and ordering

JetStream's model, not a knats choice; write handlers for it:

- **Delivery is at least once.** A handler that has not answered within `AckWait` (not `MaxProcessTime`) is presumed
  dead and the message is redelivered, possibly to another worker while the first is still running. Stay within the
  deadline, and make the handler idempotent.
- **Several instances of one durable compete**: each message goes to one instance. That is scaling, not the broadcast
  you get from several durables.
- **`Workers > 1` gives no ordering**, and with several instances even `Workers=1` does not.

knats does not deduplicate. A deduplication table outside the business transaction is false idempotence: the dedup
record commits, the business transaction rolls back, and the message never comes again. Deduplicate with the business
key, inside the business transaction.

## New durables start from new messages

A newly created durable uses `DeliverNew`: it receives only events published after it was created, so a new
subscriber does not replay the stream's history. The upstream default, `DeliverAll`, would make a new service that
subscribes to `acme.notes.>` replay every note event within retention: every notification sent again. Back-filling
history is a data migration, not something going live should trigger.

An existing durable keeps its own policy, because NATS does not allow `DeliverPolicy` to change and forcing it would
fail startup. A restarted durable resumes where it stopped; nothing published while it was down is lost.

To back-fill, create a `DeliverAll` consumer with the `nats` CLI, or run once under a different durable name: both
are explicit actions.

## Slow work goes to a task

An event handler receives, does its short work and returns. Work that takes tens of seconds becomes a task:

```go
func onNoteCreated(tasks kasynq.Enqueuer) knats.ConsumerHandler {
    return func(ctx context.Context, d *knats.Delivery) knats.HandlerResult {
        // The stream sequence does not change on redelivery, so the task is not enqueued twice.
        _, err := tasks.Enqueue(ctx, "note:index", d.Body,
            kasynq.TaskID(fmt.Sprintf("note-index-%d", d.Sequence)))
        switch {
        case err == nil, errors.Is(err, kasynq.ErrTaskIDConflict):
            return knats.OK() // a conflict means an earlier delivery already enqueued it
        default:
            return knats.RetryLater(err)
        }
    }
}
```

This keeps the handler in milliseconds, far from `MaxProcessTime`, so one slow message cannot push the next one past
its deadline. The slow work gets what tasks have and events do not: its own timeout, retry count, queue priority,
status queries and deduplication. And the failures separate: a failed enqueue redelivers the event; a failed run
retries the task and finally archives it, without touching the event stream.

The hop is not transactional. If the process dies after the enqueue and before the ack, the event is redelivered and
the task enqueued again, so the task must be idempotent or deduplicated by `TaskID` as above.

## Assembly

```go
// di.go
client, cleanup, err := knats.NewClient(cfg)
if err != nil {
    return nil, nil, err
}

// a process that publishes: create the publisher and hand it to the business layer
publisher, err := knats.NewPublisher(client, biz.NotesStream)
if err != nil {
    return nil, nil, err
}

// a process that consumes: create the consumer server and pass it to factory.New with the other servers
consumerServer, err := knats.NewConsumerServer(client, func(r knats.ConsumerRegistrar) {
    biz.NoteCreatedEvent.Handle(r, notifier.OnNoteCreated, knats.EventOptions{Durable: "notify"})
})
if err != nil {
    return nil, nil, err
}
```

`NewNamedClient` opens the client for another `nats.<name>` section. General `di.go` rules are in
[Assembly](assembly.md).

### Connection and subscriptions have separate lifetimes

`Client` owns the connection and has no close method: only the cleanup function returned by `NewClient` /
`NewNamedClient` closes it. The `keel.Server` from `NewConsumerServer` owns only its subscriptions, and its
`Stop(ctx)` never touches the connection. "Should this process still receive these events" and "should it stay
connected to NATS" are separate questions: after the consumer server stops, publishers on the same client keep
working (a task finishing during shutdown may still publish an event) until `cleanup()` runs.

### Stopping consumers: `app.stop_timeout`

Every server's `Stop(ctx)`, the consumer server's included, uses only the deadline Kratos derives from
`app.stop_timeout` (see [Configuration](config.md)). Subscriptions stop in parallel, so the total is the slowest one,
not the sum. When the deadline passes, knats stops waiting and returns an error saying so: a handler that ignores
`ctx.Done()`, a stuck downstream or a deadlock would otherwise keep the process from stopping until the orchestrator
kills it. The abandoned messages are redelivered by the server, which is better than a process that cannot stop.

Subscriptions are drained, not stopped: draining hands messages already in the client buffer to the handlers,
whereas stopping would drop them and leave them unavailable to other instances until `AckWait` expires.

### Closing the connection is a separate budget

`cleanup()` runs after `app.Run()` returns, when Kratos no longer passes any `ctx`, so `app.stop_timeout` does not
bound it. It waits at most 10 seconds for the connection drain, so acks not yet flushed are not lost with the process;
several NATS clients add up. Size the deployment's grace period (Kubernetes `terminationGracePeriodSeconds`) as:

```
grace period ≥ app.stop_timeout + 10s per NATS client
```

The defaults, 20s + 10s = 30s, come from this formula with one client.

### A consumer that dies takes the server down

When the server deletes a durable or kicks a consumer, `nats.go` just stops the subscription: the process stays up,
health checks pass and messages pile up silently. knats watches each subscription's `Closed()`; when it closes for any
reason other than shutdown, the consumer server's `Start` returns an error so the orchestrator restarts the process.

## Who creates a stream decides its config

knats creates a stream only when it does not exist. An existing stream is adopted as is: retention, replicas and
storage are left untouched, so a stream an operator created with the `nats` CLI keeps its settings, and the config
keys above only apply to streams knats creates.

knats deliberately avoids `CreateOrUpdateStream`: it pushes the code's config to the server, where a shorter
retention deletes messages on the spot and a different storage type fails. Production data is not worth a tidy
"config as code".

On adoption one thing is checked, and failing it stops startup: the stream must cover this stream's subjects
(`stream "acme_x" exists but does not cover subject ...`). A stream that does not cover them leaves consumers silently
receiving nothing, which is far harder to diagnose than a failed start. Mismatches that do not break anything (shorter
retention than configured, different replica count) are logged at WARN and the stream wins.

The cost: knats never adds subjects to an existing stream. When coverage must grow, an operator runs
`nats stream edit`; until then startup fails with the coverage error.

## No delayed delivery

knats has no delayed delivery, because NATS offers no mechanism at the right cost:

- **`NakWithDelay`** postpones a redelivery, but the message keeps occupying `MaxAckPending` while it waits (the
  server only moves the pending entry's timestamp). A 30-minute delay with a quota of 256 limits the durable to about
  0.14 messages per second, and beyond that new messages silently stall in the stream. It suits second-level retry
  backoff, which is what knats uses it for, not business scheduling.
- **Message scheduling (`Nats-Schedule`, server 2.14+)** needs a schedule entry per message in an extra stream with
  `AllowMsgSchedules`, collision-proof schedule subjects and two retention checks, and a schedule entry removed by
  retention makes its message vanish silently. Too much for an optional consumer feature.

"Check in 30 minutes whether the note was shared" is a task: [Tasks](tasks.md#task-or-event).

## Raw NATS

knats packages the common path; it is not the only door. For what it does not cover (core NATS request-reply, KV,
Object Store, custom stream config, publishing to a stream unknown at startup), take the handles from the client
instead of opening a second connection:

```go
nc := client.Conn()      // *nats.Conn
js := client.JetStream() // jetstream.JetStream
```

These handles bypass everything knats adds: no domain/stream prefix (you build subjects yourself), no trace context
(the trace breaks here), no backoff, panic recovery or dead letters, no startup stream check. Use `NewPublisher` /
`NewConsumerServer` when you want those.