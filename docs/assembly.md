# Assembly (di.go)

Read this before writing or changing a provider, or the `di.go` that wires them.

Keel uses no dependency-injection tool and ships no ProviderSet: dependencies are written by hand, in order, in the
user's own `di.go`. The style still follows wire's rules exactly: the five rules below are what wire requires of a
provider, so code that keeps them can switch to wire-generated assembly by deleting the hand-written lines.

Every Keel constructor follows the five rules. Write your data, biz and service constructors the same way, so the
whole program is one dependency graph.

## The five rules

**1. A provider is a plain function and takes every dependency as a parameter.** Its results have one of three shapes:

```go
func NewXxx(deps ...) *Xxx                      // cannot fail, nothing to release
func NewXxx(deps ...) (*Xxx, error)             // may fail
func NewXxx(deps ...) (*Xxx, func(), error)     // the second result is the cleanup
```

`keel.NewConfig` has the first shape, `kconfig.NewConfig` the second, `kdb.NewDB`, `kredis.NewRedis` and
`knats.NewClient` the third.

Process facilities are the exception: logs, traces and metrics are not in the graph, and no constructor takes them or
leaves a parameter for them. `kotel.Setup` returns `(func(), error)`, none of the three shapes, because it builds
nothing for anyone downstream to hold. See "Process facilities stay out of the graph" below.

**2. A provider does not build its own dependencies.** A component that needs Redis takes a `redis.UniversalClient`
instead of dialling: `kasynq.NewTaskClient(rdb)` accepts a `*kredis.Redis` directly, since it embeds that interface. A
self-built dependency puts the same resource in the graph twice, and only one copy gets cleaned up at shutdown.

**3. Bind by type; tell two values of the same type apart with named types.** `keel.Config` is a named type of
`kconfig.Config`, and `keel.NewConfig` does only that conversion. Without it, two `*kconfig.Config` in the graph could
not be told apart.

**4. There are no optional dependencies.** An edge is in the graph or it is not; `nil` never means "fine without it".
Split a conditional dependency into two providers and let `di.go` pick one:

```go
// node ID fixed in the config
ids, err := ksnowflake.NewStatic(cfg)
// or: leased from Redis, released when the process exits
ids, idsCleanup, err := ksnowflake.NewAuto(cfg, rdb)
```

The signatures differ (the second has a cleanup), so the choice is visible in `di.go`. A single
`New(cfg, rdb)` that accepts a nil `rdb` would reveal its path only at run time.

**5. `di.go` is the only assembly point, and cleanups run in reverse.** Construction order is dependency order;
cleanups release last-in first-out, so what was built last closes first.

```go
var cleanups []func()
defer func() {
    for i := len(cleanups) - 1; i >= 0; i-- {
        cleanups[i]()
    }
}()

rdb, rdbCleanup, err := kredis.NewRedis(cfg)
if err != nil {
    return nil, err
}
cleanups = append(cleanups, rdbCleanup) // collect it at once, not at the end
```

## A provider does not modify its shared inputs

Every argument a provider receives is also held by others in the graph. Modifying it (adding a field to the config,
registering a global plugin on the gorm instance, replacing a client's default header) makes the change visible
downstream: an implicit edge that bypasses the graph, absent from it, and the hardest kind of bug to trace. To
customise, return a new value so it becomes a named node in the graph.

## Process facilities stay out of the graph

Logs, traces and metrics are process facilities: set up once at startup and used everywhere, never passed around.

`keel.SetupLogger(cfg)` returns nothing, so it is not a provider in wire's sense. Call package-level
`slog.XxxContext` everywhere; do not pass a logger parameter or define a logger type.

`kotel.Setup(cfg)` installs tracing, metrics and the log pipeline as the OpenTelemetry globals and returns only a
cleanup and an error. Components (`kdb`, `kredis`, `khttpclient`, `mw.Default`, `mw.ClientDefault`) read the globals
themselves, so their constructors take no observability parameter:

```go
otelCleanup, err := kotel.Setup(cfg)
if err != nil {
    return nil, err
}
defer otelCleanup()

keel.SetupLogger(cfg)                // no otelCleanup, no providers
db, dbCleanup, err := kdb.NewDB(cfg) // likewise
```

The OpenTelemetry globals hand over lazily: a `otel.Tracer(name)` taken before `kotel.Setup` is a placeholder that the
real provider takes over in place once `Setup` registers it. So the providers in `di.go` need not be ordered strictly
after `kotel.Setup`.

## Reaching the native handle

Keel's wrappers are a practice, not the only way in. For a native capability the wrapper does not cover, go down from
the existing instance instead of opening a second connection: `kdb.DB` embeds `*gorm.DB`, `kredis.Redis` embeds
`redis.UniversalClient`; components whose method names would collide offer accessors named after the underlying type
(`kasynq`'s `Client()`, `Mux()`, `Server()`, `Scheduler()`; `knats`'s `Conn()`, `JetStream()`).

Each accessor's doc comment says which protections the native path loses (trace injection, the subject prefix, retry
backoff, parameter checks); read it before using one.

## Related

- [Layered architecture](architecture.md): which layer these constructors belong to and why.
