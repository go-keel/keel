# Keel glossary

Keel is a Go backend framework on go-kratos v3, bound to one chosen stack (GORM with MySQL or PostgreSQL, Redis,
asynq, NATS). Inside that stack it provides **mechanisms**: servers, middleware chains, observability, component
wrappers. Assembly is left to the user, and there is no abstraction layer across stacks.

## Roles

Each of the two roles has its own word; never use "consumer" for a user. Docs use the words in this table.

**User** (framework user):
A project that depends on Keel directly.
_Avoid_: consumer, downstream project

**Consumer**:
The receiving side of the message queue: a handler registered with `core/knats` and the `knats.NewConsumerServer`
that hosts it. In this project the word has **only** this meaning. NATS has its own **Consumer** (a JetStream durable
consumer, an entity on the server); docs call that a **durable**, not a consumer.
_Avoid_: "consumer" for a user or a JetStream durable consumer

## Framework concepts

**Mechanism**:
A capability within the chosen stack that assumes no particular use: interfaces, middleware slots, handler
registration. Keel provides mechanisms, not specific approaches.

**Event**:
A fact that has already happened, published to a stream in `core/knats`. It can have zero or more subscribers, each
consuming the same message independently. The publisher knows neither who subscribes nor how many. Events are
real-time: there is no replay and no delay (`docs/messaging.md`).

**Task**:
Work to be done, enqueued to `core/kasynq`, with a definite executor. When to choose a task over an event:
`docs/tasks.md`.

**Stream contract**:
The definition of one stream, with several event contracts under it; usually one per business domain.
_Avoid_: stream name constant, stream name string

**Event contract**:
The binding of one kind of event to its stream contract, subject and payload type, shared by publisher and
subscriber.
_Avoid_: event type, subject constant

**Publisher**:
A process's single way out for events, limited to the stream contracts it was created with.
_Avoid_: sender, producer

**Task contract**:
The binding of one kind of task to its name and payload type, shared by the enqueuing side and the executor.
_Avoid_: task type

**Dead letter**:
A record of messages that could not be processed, kept for investigation; **not** a retry mechanism (where each kind
is kept: `docs/messaging.md`, `docs/tasks.md`).

**Specific approach**:
A finished implementation that has made trade-offs for one scenario (a permission model, a kind of one-time code).
Putting it in the framework would force it on every user, so it stays out of Keel.

**Assembly**:
The order of dependency construction that the user writes by hand in `di.go`. The rules are in
`docs/assembly.md`.
_Avoid_: dependency injection, DI (this project has no DI tool)

**Middleware chain**:
A named chain such as `mw.Default`, changed by adding, removing or replacing middleware at the `mw.Slot*` constants.
_Avoid_: interceptor, filter

**Authentication**:
Checking the credential a request carries to establish who the caller is. Permission checks are a separate thing,
and a specific approach outside Keel.
_Avoid_: authorization (for this), login check

**Contract endpoint**:
An endpoint declared in proto, encoded and decoded by the framework and run through the full middleware chain;
unary and streaming alike.
_Avoid_: class 1, ordinary endpoint

**Raw route**:
A route that bypasses the middleware chain, where a handler deals with the HTTP request directly (downloads,
free-form push, callbacks).
_Avoid_: class 2, raw address, raw handler

**Decimal string**:
How a number with a fractional part (amount, rate, quantity) is written in an interface: a plain string field whose
format is guaranteed by a shared rule (`docs/proto.md`).
_Avoid_: Decimal type, wrapper type, money type

**Process facility**:
A capability that is not in the dependency graph, is set up once at startup and is taken directly wherever it is
needed: logs, traces, metrics.
_Avoid_: global variable (when meaning these three)

**Dev overlay**:
A local config file used only during development and layered over the main config (`docs/config.md`).
_Avoid_: local override, local overlay, dev config

**Liveness check**:
A probe that answers "is this process alive and able to respond", not "is everything it depends on available".
_Avoid_: health check, readiness check (both suggest checking dependencies; gRPC's own Health service is a foreign
name and is used as is)
