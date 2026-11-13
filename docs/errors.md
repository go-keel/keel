# Errors

Read this when an error starting with `keel/<id>` stops your process, or before matching such an error in code.

Every developer-facing error Keel returns (at startup, during assembly, at registration, or when a framework API is
misused) carries `keel/<id>` (a constructor may put its own prefix before it) and ends with `See
docs/errors.md#<id>`. Each ID has a section here: what triggers it, why Keel refuses instead of carrying on, and
the fix. Match these errors in code with `errors.As` on `*rule.Error` and compare its `ID`, or with `errors.Is` on the
exported sentinel where one is named below; never on the message text.

Runtime failures (a database that is down, a timeout, a decode error, a shutdown that gave up waiting) are not listed:
they are ordinary errors without an ID.

## app.no-servers

`AppFactory.New` was called without any transport server. An app with nothing to serve would start and exit at once,
or hang with no listener, so Keel refuses at assembly.

Fix: pass every server the process runs, such as the ones from `api.NewHTTPServer`, `api.NewGRPCServer`,
`kasynq.NewWorkerServer` or `knats.NewConsumerServer`.

## app.config-missing

`AppFactory` was built from a nil config, or from one without an `app` section. The app ID, name, version and stop
timeout all come from that section.

Fix: build the config with `kconfig.NewConfig` and make sure `configs/config.yaml` has an `app` section. See
[the config component](config.md).

## client.invalid-params

`keel.NewServiceClient` was called with a nil factory, a factory without config, an empty service name, or a nil
`newClient`. None of them has a usable default.

Fix: pass the factory from `keel.NewServiceClientFactory`, the service name as written under `client` in the config,
and the generated `New<Service>Client` function. See [the service client](client.md).

## client.config-missing

`keel.NewServiceClient` found no `client.<name>` section for the service name it was given, so it has no endpoint to
dial.

Fix: add `client: {<name>: {endpoint: ...}}` to the config, or pass the name that already exists there. See
[the service client](client.md).

## kconfig.load

`configs/config.yaml` (or the file given to `kconfig.NewDynamic`) could not be read or parsed as YAML. The underlying
reason is in the message and stays in the error chain.

Fix: run the process from the directory that holds `configs/`, and fix the file so it is readable YAML. See
[the config component](config.md).

## kconfig.scan

The config file parsed as YAML but could not be decoded into the schema: a value has the wrong type for its key. The
schema for the main config is `conf/conf.proto`; for `kconfig.NewDynamic` it is the type you passed.

Fix: change the key or value named in the message so it matches the schema.

## kconfig.dev-overlay-in-prod

A dev overlay (`configs/config.dev.yaml`, or `<name>.dev.<ext>` next to a dynamic config) exists while `KEEL_ENV=prod`.
The overlay carries development placeholders such as dev secrets; in production they would silently override the
real config, so Keel refuses to start instead of only logging a warning.

Fix: remove the dev overlay from this deployment. See [the config component](config.md).

## kconfig.env-invalid

`KEEL_ENV` has a value other than `dev` or `prod`. Writing `production` or `PROD` would look like it enables the
production safeguards while enabling none of them, so any other value is refused.

Fix: set `KEEL_ENV` to `dev` or `prod`, or leave it unset for `dev`.

## kconfig.empty

`(*kconfig.Config).Validate` was called on a `kconfig.Config` whose embedded `*conf.Config` is nil. This only happens
when a config is built by hand (usually in a test) instead of through `kconfig.NewConfig`.

Fix: build the config with `kconfig.NewConfig`, or set `Config` before calling `Validate`.

## kconfig.invalid

The config loaded but breaks one or more protovalidate rules declared in `conf/conf.proto` (or in the `.proto` of the
type given to `kconfig.NewDynamic`). Constraints on config values live only there and are checked once at load, so a
wrong value fails at startup instead of when it is first used. The message lists every failing field.

Fix: correct each listed field so it satisfies its rule. See [the config component](config.md).

## kdb.config-missing

`kdb.NewDB` or `kdb.NewNamedDB` found no `database` section, or no `database.<name>` section for the name it was
given.

Fix: add `database: {<name>: {driver: ..., source: ...}}` to the config, or pass the name that already exists there.
See [the database component](database.md).

## kdb.driver-missing

`database.<name>.driver` names a database whose dialect package is not imported. `kdb` imports no driver itself, so a
program that uses one database does not compile in the other; the dialect registers itself through a blank import.

Fix: add the import the message names to package main: `import _ "github.com/go-keel/keel/core/kdb/mysql"` or
`import _ "github.com/go-keel/keel/core/kdb/postgres"`. See [the database component](database.md).

## kdb.source-invalid

`database.<name>.source` does not parse as a DSN for its driver. The parser's reason is in the message and stays in the
error chain.

Fix: rewrite the source in the DSN format of that driver. See [the database component](database.md).

## kredis.config-missing

`kredis.NewRedis` or `kredis.NewNamedRedis` found no `redis` section, or no `redis.<name>` section for the name it was
given.

Fix: add `redis: {<name>: {addr: [...]}}` to the config, or pass the name that already exists there. See
[the Redis component](redis.md).

## kredis.lock-ttl

`TryAcquireLock` was called with a `ttlSec` of zero or less. Without a TTL the lock never expires, so a holder that
crashes keeps it forever.

Fix: pass a positive `ttlSec`, longer than the work the lock protects. See [the Redis component](redis.md).

## kredis.ratelimit-bucket

`ratelimit.New` was given a bucket duration of zero or less; windows are counted in buckets, so there would be nothing
to count with.

Fix: pass a positive bucket, such as `time.Minute`. See [the Redis component](redis.md).

## kredis.ratelimit-window

`(*ratelimit.Limiter).Allow` was given a window that is not a whole number of the limiter's buckets (shorter than one
bucket included). Rounding up would count a longer span and reject requests the window allows; rounding down would
silently shorten the window. Keel refuses either.

Fix: pass a window that is a multiple of the bucket, or create the limiter with a bucket that divides the window. See
[the Redis component](redis.md).

## kredis.ratelimit-limit

`(*ratelimit.Limiter).Allow` was given a limit below 1, which would reject every request. It is usually a config value
that is missing or zero.

Fix: pass a limit of at least 1, and check the config value it comes from. See [the Redis component](redis.md).

## kasynq.config-missing

A `kasynq` constructor found no `asynq.<name>` section for the name it was given.

Fix: add `asynq: {<name>: {}}` to the config; every field in it is optional. See [task servers](tasks.md).

## kasynq.eviction-policy

The Redis that asynq uses has `maxmemory-policy` set to an `allkeys-*` policy. Under memory pressure Redis may then
evict asynq's queue keys, and tasks disappear without any error.

Fix: set `maxmemory-policy` to `volatile-lru` (cache keys must then have a TTL) or `noeviction`, or give asynq a Redis
of its own. See [task servers](tasks.md).

## kasynq.eviction-unknown

The Redis that asynq uses does not report `maxmemory_policy` in `INFO memory`, so Keel cannot check that it will not
evict asynq's queues (see `kasynq.eviction-policy`).

Fix: point asynq at a Redis server that reports `maxmemory_policy` in `INFO memory`. See [task servers](tasks.md).

## kasynq.scheduler-lease-lost

The cron scheduler lost its Redis lease: no renewal succeeded for 20s, or another instance holds it. It stopped
scheduling before the lease could expire, so two replicas never enqueue the same trigger, and the process exits.

Fix: check that Redis is reachable from this instance, and that only replicas of this application use the same
`app.domain` and `app.name`. The restarted process waits for the lease again. See [task servers](tasks.md#one-replica-schedules).

## kasynq.zero-option

An `EnqueueOption` is the zero value `EnqueueOption{}`, which only happens when an option is built without its
constructor. asynq would silently ignore it on enqueue.

Fix: build every option with its constructor, such as `kasynq.MaxRetry` or `kasynq.Queue`.

## kasynq.queue-empty

A cron registration passes `Queue` with an empty or blank name. asynq would only reject it when the cron fires, so the
app would start and the task would never be enqueued.

Fix: pass a queue name listed under `asynq.<name>.queues`, or drop the `Queue` option to use the default queue. See
[task servers](tasks.md).

## kasynq.task-id-empty

A cron registration passes `TaskID` with an empty or blank ID. As with `kasynq.queue-empty`, asynq would only reject it
when the cron fires.

Fix: pass a non-empty ID to `TaskID`, or drop the option.

## kasynq.unique-ttl

A cron registration passes `Unique` with a TTL under one second, which asynq rejects only when the cron fires.

Fix: pass `Unique` a TTL of at least `time.Second`.

## kasynq.task-type-empty

A cron task is registered with an empty or blank task type name. asynq checks only the cron spec at registration, so
the app would start and the task would never run.

Fix: pass the task type name the worker handles, or register through `TaskType.RegisterCron`. See
[task servers](tasks.md).

## kasynq.register-failed

Registering a task handler failed, because the task type is empty or was already registered. asynq panics in this
case; Keel turns the panic into this error so it comes back from the constructor with the others.

Fix: give each handler a non-empty task type and register each type once. See [task servers](tasks.md).

## knats.config-missing

`knats.NewClient` or `knats.NewNamedClient` found no `nats.<name>` section for the name it was given.

Fix: add `nats: {<name>: {url: ...}}` to the config, or pass the name that already exists there. See
[the NATS component](messaging.md).

## knats.domain-missing

`app.domain` is empty. knats builds stream names and subject prefixes from it, so without it publishers and consumers
of different services would collide.

Fix: set `app.domain` in the config. See [the NATS component](messaging.md).

## knats.stream-subjects

A stream already exists on the server but does not cover a subject this service needs. Messages on that subject would
never reach the stream, silently.

Fix: add the subject to the stream with the nats CLI, or delete the stream and let knats recreate it. See
[the NATS component](messaging.md).

## knats.stream-undeclared

`Publish` / `PublishEx` named a stream that was not passed to `NewPublisher`. Streams are checked when the publisher is
created, so an undeclared one has never been verified. Sentinel: `knats.ErrStreamNotDeclared`.

Fix: add the stream to the `NewPublisher` call that built this publisher. See [the NATS component](messaging.md).

## knats.no-streams

`knats.NewPublisher` was called without any stream, so nothing could ever be published through it. Sentinel:
`knats.ErrNoStreams`.

Fix: pass every stream this process publishes to, such as `knats.NewPublisher(client, OrderStream)`.

## knats.option-negative

A consumer's `Workers`, `MaxProcessTime` or `MaxDeliver` is negative. Zero means "use the default"; a negative value has
no meaning and would otherwise be passed through. Sentinel: `knats.ErrNegativeOption`.

Fix: set the field to a positive value, or to 0 for the default. See [the NATS component](messaging.md).

## knats.subject-prefixed

A subject already starts with the `<domain>.<stream>.` prefix that knats adds itself. The doubled prefix is valid
syntax, so publishers and consumers would silently never meet. Sentinel: `knats.ErrSubjectPrefixed`.

Fix: pass the business topic only, such as `order.paid`. See [the NATS component](messaging.md).

## knats.stream-name

A stream name contains `.`. NATS rejects such stream names; Keel reports it when the publisher or consumer is set up
instead of after a round trip to the server. Sentinel: `knats.ErrStreamNameDotted`.

Fix: use a stream name without dots, such as `order_events`.

## knats.stream-empty

A `knats.Stream` has an empty name, so no stream name or subject can be built from it. Sentinel:
`knats.ErrEmptyStream`.

Fix: give `knats.NewStream` a non-empty name.

## knats.subject-empty

`Publish` / `PublishEx` was called with an empty subject. Sentinel: `knats.ErrEmptySubject`.

Fix: pass the business topic as the subject, such as `order.paid`.

## knats.durable-empty

A consumer has an empty `ConsumerOptions.Durable`. The durable name is what keeps a consumer's progress across
restarts. Sentinel: `knats.ErrEmptyDurable`.

Fix: set `Durable` to a name that is unique on that stream. See [the NATS component](messaging.md).

## knats.durable-duplicate

Two consumers on the same stream use the same durable name. They would share one progress cursor and each receive half
of the messages. The same name on different streams is fine. Sentinel: `knats.ErrDuplicateDurable`.

Fix: give each consumer on a stream its own `Durable`.

## knats.handler-nil

`Register` was given a nil `ConsumerHandler`. Sentinel: `knats.ErrNilHandler`.

Fix: pass a non-nil handler.

## knats.no-consumers

The `ConsumerSetup` given to `knats.NewConsumerServer` registered no consumer, so the server would run doing nothing.
Sentinel: `knats.ErrNoConsumers`.

Fix: call `Register` in the setup, or remove the consumer server from `di.go`.

## ksnowflake.allocator-mismatch

The snowflake constructor chosen in `di.go` does not match `app.node_id`: `ksnowflake.NewStatic` needs a fixed ID in
[0, 1023], `ksnowflake.NewAuto` needs `-1`. The choice is made in `di.go` so that only leased node IDs pull in Redis.

Fix: use the constructor that matches `app.node_id`, or change `app.node_id`. See [snowflake IDs](utilities.md).

## khttpclient.endpoint-invalid

`khttpclient.New` was given an endpoint that is not an absolute `http://` or `https://` URL with a host.

Fix: pass the base URL of the service, such as `http://user-svc:8000`. See [the HTTP client](http-client.md).

## khttpclient.h2c-scheme

`WithH2C` was set on an `https://` endpoint. h2c is HTTP/2 without TLS; over TLS, HTTP/2 is negotiated on its own.

Fix: use an `http://` endpoint with `WithH2C`, or drop `WithH2C`. See [the HTTP client](http-client.md).

## khttpclient.body-limit

`WithResponseBodyLimit` was given zero or a negative number. There is no "no limit" setting: an unbounded response body
can exhaust memory.

Fix: pass a positive byte limit large enough for the expected responses; large downloads belong to another channel.
See [the HTTP client](http-client.md).

## khttpclient.nil-result

`khttpclient.ProtoResult` was given a nil message (or a typed nil pointer), which protojson cannot decode into. This is
the caller's mistake, not the remote service's, so it is not reported as a contract error.

Fix: pass a pointer to a message, such as `ProtoResult(&pb.User{})`. See [the HTTP client](http-client.md).

## mw.slot-not-found

`InsertAfter`, `InsertBefore` or a preset named a slot that the chain does not have. Chains collect such errors and
the server constructor or `keel.NewServiceClient` returns them all at once, instead of panicking at the first one.

Fix: use a `mw.Slot*` constant or a name given to an earlier insert, and build the chain from the preset that has that
slot. See [middleware chains](middleware.md).

## mw.no-auth-slot

The server has auth configured but its chain has no auth slot, for example because it was built from `mw.Minimal`.
The configured auth would otherwise silently not run.

Fix: build the server's chain from `mw.Default`, or remove the auth section from that server's config. See
[the auth middleware](auth.md).

## mw.slot-duplicate

A preset was mounted twice on one chain (for example `WithSentinel` called twice), or `InsertAfter` / `InsertBefore`
named the new slot with a name the chain already has. The middleware would run twice, and by-name operations would
only reach the first copy.

Fix: mount each preset once per chain, and give each inserted middleware a name no other slot uses. See
[middleware chains](middleware.md).

## mw.locale-bundle-missing

`mw.WithLocale` was given a nil `*locale.Bundle`, or one whose inner `i18n.Bundle` is nil. Every request would panic
on it.

Fix: pass the bundle built by `locale.NewBundle` from a loaded `i18n.Bundle`, or drop the `WithLocale` call. See
[middleware chains](middleware.md).

## mw.sentinel-group-missing

`mw.WithSentinel` was given a nil `*sentinel.Group`.

Fix: pass a group built with `sentinel.NewGroup`, or drop the `WithSentinel` call. See
[middleware chains](middleware.md).

## sentinel.rule-invalid

A `sentinel.Group` registration had a nil sentinel error or a nil handler (`On`, `OnLocaled`, `Default`). The mapping
would never run, so the business error would reach the caller as an internal error. `mw.WithSentinel` records the
group's errors on the chain.

Fix: pass a non-nil sentinel error and a non-nil handler, or drop the call. See [middleware chains](middleware.md).

## api.http-config-missing

`api.NewHTTPServer` was given a nil config, usually because `server.http.<name>` was read with a name that has no
section.

Fix: add the section under `server.http` in the config, or pass the name that exists. See
[the HTTP server](http.md).

## api.grpc-config-missing

`api.NewGRPCServer` was given a nil config, usually because `server.grpc.<name>` was read with a name that has no
section.

Fix: add the section under `server.grpc` in the config, or pass the name that exists. See
[the gRPC server](grpc.md).

## api.auth-algorithm

A server's `auth.algorithm` names a JWT algorithm Keel does not support.

Fix: set `auth.algorithm` to `RS256` (the default) or `HS256`. See [the auth middleware](auth.md).

## api.trusted-proxy-invalid

An entry in `server.http.<name>.client_ip.trusted_proxies` is not an IP address or a masked CIDR range (an address with
a zone, or a range with host bits set, also counts). Dropping it silently would trust fewer proxies than written.
A config loaded through `kconfig.NewConfig` reports the same entry earlier as `kconfig.invalid`; this ID appears when
the server is built from a config that skipped it, usually in a test.

Fix: write each entry as an IP address or a masked CIDR range without a zone, such as `10.0.0.0/8`. See
[metadata and the visitor address](metadata.md).

## api.stream-filter-nil

`StreamContext.Filter` was called with a nil filter.

Fix: pass a non-nil `func(http.Handler) http.Handler`, or drop the call. See [the HTTP server](http.md).

## api.require-login-without-auth

A raw route was registered through `StreamContext.RequireLogin` on a server that has no auth configured. There is no
verifier to check the credential, so the route would be open while declaring that it requires login.

Fix: enable auth in that server's config, or register the route through `Anonymous`. See
[the HTTP server](http.md).

## api.sse-no-flush

`api.NewSSEWriter` was given an `http.ResponseWriter` that does not implement `http.Flusher`, usually a wrapper from
your own middleware. Without flushing, events would be buffered instead of streamed.

Fix: pass the writer the server handed to the handler, or make the wrapper implement `Flush`. See
[the HTTP server](http.md).

## api.health-route-conflict

Your proto or a raw route also uses `/health`, which Keel registers on every HTTP server for liveness checks. The mux
would silently let one route hide the other, so Keel refuses to start.

Fix: remove your own route; Keel already answers liveness checks at `/health`, and only liveness: it does not probe
dependencies, so a dependency blip does not take every instance out of the load balancer at once.

## jwt.key-file-unreadable

`jwt.LoadRSAPrivateKey` or `jwt.LoadRSAPublicKey` could not read the key file; the error names the path. For a
server's auth, the error is prefixed with `auth.jwt_public_key_file`.

Fix: check the path in the config (relative paths start from the working directory) and the file's permissions. See
[utilities](utilities.md#jwt-pkgjwt).

## jwt.private-key-invalid

The private key file is not a PEM-encoded RSA private key (PKCS#1 `RSA PRIVATE KEY` or PKCS#8 `PRIVATE KEY`).

Fix: point to the private key, such as one made by `openssl genrsa -out private.pem 2048`. See
[utilities](utilities.md#jwt-pkgjwt).

## jwt.public-key-invalid

The public key file is not a PEM-encoded RSA public key: PKIX `PUBLIC KEY`, PKCS#1 `RSA PUBLIC KEY` or an X.509
certificate.

Fix: write the public key with `openssl rsa -in private.pem -pubout -out public.pem`. See
[utilities](utilities.md#jwt-pkgjwt).

## redact.invalid-annotation

`redact.Validate` found a `keel.v1.redact` annotation on a field whose value cannot be replaced by the mask (anything
other than `string`, `repeated string` or a map with `string` values). Such a field would be logged unredacted, so the
check runs at startup.

Fix: move the annotation to a string field. See
[request logging](middleware.md).
