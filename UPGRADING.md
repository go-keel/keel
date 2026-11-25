# Upgrading

Read this on every upgrade of `github.com/go-keel/keel`. It lists only the changes that need action from users: a
changed public API, a changed default, a config or calling-code change. Whether a change is listed depends only on
that, not on which part of the version moved: a patch release can need action too. New capabilities, internal
refactors and bug fixes are not listed; see the commits.

Newest entries go on top. Each entry is anchored to a version and says three things: **what changed**, **why**, **how
to apply**. The core module is versioned with tags; optional modules (`core/kdb/mysql`, `core/kdb/postgres`,
`core/kasynq`, `core/knats`, `core/khttpclient`) follow the core's commit with pseudo-versions: after upgrading the
core, fetch each optional module you use again at the new tag's commit (`docs/quickstart.md`, "Add the dependency").

## v0.2.0

### `locale.Bundle` is gone; `OnLocaled` is replaced by `OnMessage` and `locale.Localize`; validation messages are protovalidate's text

**What changed:**
- `locale.Bundle` and `locale.NewBundle` are gone: `mw.WithLocale` and `locale.Server` take go-i18n's `*i18n.Bundle`.
- `sentinel.Group.OnLocaled` is removed. `OnMessage(sentinel, msg, build)` covers the common case;
  `locale.Localize(ctx, msg)` translates with fallback anywhere else.
- A `VALIDATOR` error's message is protovalidate's text, naming the field and the constraint, in English, as Kratos's
  validate middleware returns it. Before, it was one translated sentence per kind of violation (English, Chinese,
  Japanese) with no field name.

**Why:** one way to load, match and translate. The wrapper added nothing over go-i18n; every service rewrote the same
"translate, fall back to English" handler. A validation failure means the calling code skipped a check its client
should make, so the message is for that code's developer, who needs the field name; translating it was Keel choosing
languages for its users.

**How to apply:**
- Pass the go-i18n bundle itself to `WithLocale` (drop the `locale.NewBundle(b)` wrapping).
- Replace each `OnLocaled(err, handler)` that only translates a message into a generated error with
  `OnMessage(err, &i18n.Message{ID: ..., Other: ...}, v1.ErrorXxx)`; others become `On(err, handler)` calling
  `locale.Localize` or `locale.FromContext`.
- Clients that showed the `VALIDATOR` message to end users: check the input in the client first, and show your own
  text for a `VALIDATOR` error.

### Event and task spans use `messaging.operation.type`

**What changed:** Keel's tracing follows OpenTelemetry semantic conventions v1.40.0 everywhere (it mixed v1.24.0 and
v1.40.0). On knats and kasynq spans, `messaging.operation` (`publish` / `deliver`) is replaced by
`messaging.operation.type` (`send` on the sending span, `process` on the handling span). The `kotel` resource
attributes (`service.name`, `service.version`, `host.name`) keep their names.

**Why:** one semantic-conventions version keeps attribute names consistent across database, Redis and messaging
spans; v1.40.0 renamed the messaging operation attribute and its values.

**How to apply:** update trace queries, dashboards and alerts that filter on `messaging.operation` to
`messaging.operation.type`, with `publish` becoming `send` and `deliver` becoming `process`.

### `api.WebSocketHandler` takes one `WSOptions`; `logging.Options` is gone

**What changed:** `api.WebSocketHandler(handler, opts ...WSOptions)` is `api.WebSocketHandler(handler, opts WSOptions)`;
the zero value keeps every default. The `logging.Options` struct is unexported; configure the logging middleware only
with the `logging.WithXxx` options.

**Why:** each component configures one way: a struct where options are plain values (`WSOptions`,
`DownloadOptions`), functional options where Kratos uses them (`logging`). A variadic struct whose extra elements were
ignored and a struct that duplicated the `WithXxx` functions were two more ways.

**How to apply:** add `api.WSOptions{}` as the last argument where `WebSocketHandler` was called without options.
Replace any use of `logging.Options` with the matching `logging.WithXxx` calls.

### Lock TTLs are `time.Duration`

**What changed:** `lock.Locker.TryAcquireLock(ctx, key, ttl)` and `TryAcquireLockWithTimeout(ctx, key, ttl, timeout)`
take `ttl time.Duration` instead of `ttlSec int`. A TTL under 1ms fails with `keel/kredis.lock-ttl`.

**Why:** every other duration in Keel is a `time.Duration`, and an integer of seconds next to a `time.Duration`
timeout in the same call was easy to mix up.

**How to apply:** multiply each TTL by `time.Second`: `TryAcquireLock(ctx, key, 30)` becomes
`TryAcquireLock(ctx, key, 30*time.Second)`. A bare integer still compiles, because an untyped constant converts to
`time.Duration`, but it now means nanoseconds: `30` is 30ns, which the 1ms floor rejects at the first call. Search
for every `TryAcquireLock` call rather than relying on the compiler.

### Constructors return concrete types; `worker.Server` is `worker.NewServer`

**What changed:** constructors that returned an interface now return the type they build:
`lock.NewLocker` returns `*lock.Locker` (a struct; the `Locker` interface is gone), `knats.NewPublisher` returns
`*knats.StreamPublisher`, `knats.NewConsumerServer` returns `*knats.ConsumerServer`, and the `jwt` constructors
return `*jwt.RS256Signer`, `*jwt.HS256Signer`, `*jwt.RS256Verifier` and `*jwt.HS256Verifier`. `worker.Server(runner)`
is renamed `worker.NewServer(runner)` and returns `*worker.Server`. The `knats.Publisher`, `kasynq.Enqueuer`,
`jwt.Signer` and `jwt.Verifier` interfaces stay. `api.NewHTTPServer` and `api.NewGRPCServer` still return
`keel.Server`.

**Why:** accept interfaces, return concrete types: a caller that needs a test double defines the small interface it
uses, and the constructor no longer hides methods behind one.

**How to apply:** replace `worker.Server(` with `worker.NewServer(`. Where a field, parameter or provider result was
typed `lock.Locker`, type it `*lock.Locker`, or declare an interface with the methods you call in the package that
uses it (needed for a fake). Fields typed `knats.Publisher`, `jwt.Signer` or `jwt.Verifier` keep working; a
`keel.Server` variable holding a consumer server or worker keeps working too.

### `metadata.Get` and `metadata.Set` replace `metadata.Metadata`

**What changed:** `middleware/metadata.Metadata` (with `AcceptLanguage`, `RemoteAddress` and `Get`) and
`metadata.FromServerContext` are gone. The package's whole API is `GetRemoteIP`, `GetAcceptLanguage`,
`Get(ctx, key)` and `Set(ctx, key, value) (context.Context, error)`. `Set` copies the request's metadata into a new
ctx and refuses a key that does not start with `x-keel-` (`keel/metadata.key-prefix`).

**Why:** the type duplicated Kratos's own `metadata` entry points, and its `RemoteAddress()` skipped the fallback
`GetRemoteIP` applies outside the chain. One entry point leaves one way to read or write a key.

**How to apply:** replace `md, _ := metadata.FromServerContext(ctx); md.Get(k)` with `metadata.Get(ctx, k)`,
`md.RemoteAddress()` with `metadata.GetRemoteIP(ctx)` and `md.AcceptLanguage()` with `metadata.GetAcceptLanguage(ctx)`.
Where you wrote a key through Kratos (`kmd.FromServerContext(ctx)` then `md.Set`, or `kmd.NewServerContext` in a
test), use `ctx, err := metadata.Set(ctx, key, value)` and keep the returned ctx.

### The validate slot runs protovalidate only

**What changed:** the validate middleware in `mw.Default` no longer calls a request's own `Validate() error` method
(the protoc-gen-validate style); it runs only the protovalidate rules declared in the proto.

**Why:** field validation has one way to be written, protovalidate rules in the proto; protoc-gen-validate is
superseded by protovalidate.

**How to apply:** if a request type still relies on a generated or hand-written `Validate()` method, move those
constraints into `buf.validate` rules on its proto fields. Requests generated without such a method are unaffected.

### `pkg/json` is `encoding/json` (v1), and events encode with it

**What changed:** `pkg/json` wraps `encoding/json` instead of `encoding/json/v2`, and it is the only JSON codec Keel
uses itself: knats event payloads (already through `pkg/json`), kasynq task payloads and khttpclient bodies (both
`encoding/json` before). What you see differ is the v2 to v1 change in event payloads and anywhere you call
`pkg/json`: a nil slice or nil map encodes as `null` instead of `[]` / `{}`; `<`, `>`, `&` are escaped as
`\u003c`, `\u003e`, `\u0026`; and a field tagged `omitempty` is left out when it holds a zero value (`0`, `false`,
a nil pointer) instead of only when its JSON would be empty, so `0` and `false` no longer appear while a pointer to
an empty struct now appears as `{}`. Decoding is more lenient (field names match case-insensitively, duplicate keys are
accepted). Empty input still unmarshals to a nil error.

**Why:** two codecs meant a value encoded one way as an event and another way as a task; v1 is what the rest of the
Go ecosystem and the standard library docs assume.

**How to apply:** a consumer in another language that reads an event field as an array or object must accept `null`
for an empty one, or initialise the slice or map before publishing; one that expects an `omitempty` number or
boolean to be present must accept its absence, or drop `omitempty` from the tag. Tests that compare `pkg/json` output with a
literal containing `[]`, `{}` or HTML characters need the new form. Events and tasks already in a stream or queue
decode unchanged.

### An unset `app.domain` means `keel` in knats too

**What changed:** `knats.NewClient` no longer fails with `knats.domain-missing` when `app.domain` is empty; it uses
`keel`, the default `ksnowflake` and the `kasynq` scheduler already used. The rule ID is gone.

**Why:** the three components disagreed: two fell back to `keel` silently, one refused to start. One default, defined
once, keeps stream names and Redis keys in one namespace.

**How to apply:** nothing, unless a test expects `knats.domain-missing`: delete that test or set `app.domain` and
expect startup to succeed. A service that relied on the error to catch a forgotten `app.domain` should set it.

### `keel.NewApp` and `keel.NewServiceClient` take `*kconfig.Config`

**What changed:** `keel.Config`, `keel.NewConfig`, `keel.AppFactory`, `keel.NewAppFactory` and
`keel.ServiceClientFactory` / `keel.NewServiceClientFactory` are gone. Build the app with
`keel.NewApp(cfg, servers...)` and clients with `keel.NewServiceClient(cfg, name, newClient, chain)`, both taking the
`*kconfig.Config` from `kconfig.NewConfig`. `mw.Default` and `mw.ClientDefault` take `*kconfig.Config` instead of
`*conf.Config`.

**Why:** the same config had three types (`conf.Config`, `kconfig.Config`, `keel.Config`) and two factories that only
held it, so `di.go` converted it back and forth. Every constructor now takes the one loaded config.

**How to apply:** in `di.go` and `main.go`, replace `keel.NewAppFactory(keel.NewConfig(cfg)).New(servers...)` (or
`factory.New(...)`) with `keel.NewApp(cfg, servers...)`; delete the `keel.NewServiceClientFactory` provider and pass
`cfg` where a client constructor took the factory; change `mw.Default(cfg.Config)` and
`mw.ClientDefault(factory.Config().Config)` to `mw.Default(cfg)` and `mw.ClientDefault(cfg)`. `go build` lists every
call site.

## v0.1.1

### `auth.NewContext` takes `jwtv5.MapClaims`

**What changed:** `middleware/auth.NewContext(ctx, claims)` takes `jwtv5.MapClaims` instead of any `jwtv5.Claims`.

**Why:** `auth.FromContext` returns only `jwtv5.MapClaims`. Claims of any other type were stored without error and then
read back as "not signed in" everywhere, so a server with its own authentication middleware lost its callers silently.

**How to apply:** where you call `auth.NewContext` with another claims type (such as `jwtv5.RegisteredClaims` or your
own struct), build a `jwtv5.MapClaims` with the same fields instead. Calls that already pass `jwtv5.MapClaims` compile
unchanged.
