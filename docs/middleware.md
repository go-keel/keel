# Middleware chains

Read this before building a server or client chain, adding your own middleware, turning on body logging, adding
translations, or mapping business errors to responses.

The `mw` package builds a middleware chain: an ordered list of middleware, each in a named slot. A server takes the
chain in its constructor; contract endpoints run all of it, raw routes and `/health` skip it ([HTTP](http.md)).

## Building a chain

Start from a preset and add framework middleware only through the `mw.With*` functions. Each one puts its middleware in
the slot where it works. It modifies the chain in place and returns nothing, so each call is its own statement:

| Function | What it adds | Where |
|---|---|---|
| `mw.Default(cfg)` | recovery → tracing → metrics → logging → metadata → auth → validate | server base |
| `mw.Minimal()` | recovery → logging; no auth slot | servers that need no full chain |
| `mw.WithRatelimit(chain, opts...)` | Kratos BBR adaptive limiter | right after recovery, before tracing and logging |
| `mw.WithLocale(chain, bundle)` | localizer in the context | right after metadata |
| `mw.WithTrustedMetadata(chain)` | metadata that keeps the inbound client IP | replaces metadata; internal servers only ([metadata](metadata.md)) |
| `mw.WithLogging(chain, opts...)` | logging with your options | replaces logging |
| `mw.WithSentinel(chain, group)` | business error → transport error | appended last, closest to the handler |
| `mw.ClientDefault(cfg)` | recovery → tracing → metrics → metadata → circuit breaker | client base |
| `mw.WithClientAuth(chain, token)` | `Bearer <token()>` on every call | client auth slot |

```go
chain := mw.Default(cfg)
mw.WithRatelimit(chain)
mw.WithLocale(chain, bundle)
mw.WithSentinel(chain, noteErrors)
```

- The auth slot in `Default` is a pass-through placeholder. `api.NewHTTPServer` / `NewGRPCServer` clone the chain and
  fill it from the server's own `auth` config; you never fill it yourself ([auth](auth.md)). A server with auth enabled
  and a `Minimal` chain fails startup (`mw.no-auth-slot`), because its configured auth would silently not run.
- `Default` and `ClientDefault` take no observability arguments: tracing and metrics use the global OpenTelemetry
  providers set by `kotel.Setup`, which are harmless no-ops when it was not called ([observability](observability.md)).
- `WithLocale` with a nil bundle, `WithSentinel` with a nil group or a group with an invalid registration, and
  mounting a preset (or inserting a slot name) twice fail startup through the chain error, because each would
  otherwise panic on every request, drop a mapping or run a middleware twice.
- `Default` runs `redact.Validate`, so a `keel.v1.redact` annotation that cannot take effect fails startup
  ([proto](proto.md)).
- BBR tunes itself from CPU and in-flight requests and needs no configuration. It counts a long-lived stream as one
  request for the stream's whole life, so keep streaming endpoints off rate-limited servers.

Presets and inserts modify the chain in place and return nothing, so there is one way to write a change and no
chained call can be mistaken for a new chain. Build one chain per server; to derive variants from a shared base, `Clone()` it
first, or two servers customizing the same chain would corrupt each other.

## Adding your own middleware

Your own middleware goes in only through `InsertAfter` / `InsertBefore`, anchored on a slot constant (`mw.Slot*`,
`mw.ClientSlot*`) or a name you gave an earlier insert. The chain has no public add, replace or remove, so framework
slots keep their order and cannot be dropped by accident. `mw.Middleware` is an alias of the Kratos middleware type,
so Kratos ecosystem middleware goes in without conversion.

```go
func NoteQuota() mw.Middleware {
	return func(next middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			// before the handler
			reply, err := next(ctx, req)
			// after the handler
			return reply, err
		}
	}
}

chain := mw.Default(cfg)
chain.InsertAfter(mw.SlotValidate, NoteQuota(), "note-quota") // runs after auth and validation

adminChain := chain.Clone() // the original chain is unchanged
adminChain.InsertAfter("note-quota", auditLog(), "audit")
```

A checking step after authentication (bans, permissions) is your own middleware inserted after the auth slot:
authentication answers "who is this", not "may they do this".

A missing anchor does not panic. The error is recorded on the chain, and the server constructor or
`keel.NewServiceClient` returns every recorded error at once (`mw.slot-not-found`), because startup errors have one
exit: the constructor's return value.

## Order and dependencies

A `Default` chain with every server preset, after the server filled auth:

1. Recovery: turns a panic into an error.
2. Ratelimit (`WithRatelimit`).
3. Tracing: the request span.
4. Metrics: count and latency.
5. Logging: one record per request.
6. Metadata: Accept-Language, client IP, `x-keel-*` keys ([metadata](metadata.md)).
7. Locale (`WithLocale`): needs the language from metadata.
8. Auth: pass-through, or JWT verification when the server's auth is on. It reads the token from the request itself.
9. Validate: protovalidate on the request message; picks its message language from metadata.
10. Sentinel (`WithSentinel`): can use the localizer from locale.

## Logging

The logging slot writes one `log/slog` record per request with the trace attached. By default it records the operation,
latency, status and error, and not bodies or headers, because they may hold personal data or credentials. No path is
skipped by default.

Fields: `kind` (`server`), `component` (`http` or `grpc`), `operation` (HTTP path or gRPC method), `latency` (seconds),
`response_code` (200 / 0 on success), `error`, and when enabled `request_headers`, `request_body`, `response_body`.

The level follows the outcome: success is `info`; a failure the caller caused (4xx: not signed in, forbidden, invalid,
not found) is `warn`; a service fault (5xx) or an unrecognised error is `error`. 4xx is not `error` because it is not a
fault of the service: logging it as one buries real faults and breaks alerts that count errors, and a spike still shows
in `warn` (credential stuffing, scraping, a client bug).

Turn on bodies or headers through `mw.WithLogging`. `logging.WithSkipPaths` leaves bodies, headers and the `phase=open`
record out for the listed paths; their per-request record is still written.

```go
mw.WithLogging(chain,
	logging.WithLogRequest(true),
	logging.WithLogResponse(true),
	logging.WithLogHeaders(true),
)
```

- Body logging is opt-in. Proto fields marked `(keel.v1.redact).sensitive` are written as `"***"` and fields marked
  `bulky` as `"[omitted N bytes]"`, in requests and responses ([proto](proto.md)). Unmarked sensitive fields are logged
  as they are, and non-proto payloads have nothing to go by: check the contract is fully annotated before turning it on.
- Credential headers (`Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key`) are written as
  `[redacted]`. Turn on header logging with care all the same.
- Bodies are cut at 4 KB by default (`WithMaxRequestSize`, `WithMaxResponseSize`; 0 means no limit) on a UTF-8
  boundary, with `...[truncated]` appended, so the start survives for debugging.
- Bodies are encoded like the server encodes them (protojson for proto messages), so the log shows what the caller saw.
  `[]byte` is logged as `[binary]`. Response bodies are logged only on success; failures log `error`.
- A stream (SSE, WebSocket) writes its completion record when it ends, with the whole stream as latency, and a
  `phase=open` record when it starts, so a long connection is not invisible. SSE gets the `phase=open` record only when
  the request sends `Accept: text/event-stream` (as `EventSource` does); gRPC streams have none.

## Locale

`WithLocale` puts a go-i18n localizer for the request's Accept-Language into the context; without a matching
language, the bundle's default language is used. Build the bundle from go-i18n and wrap it with `locale.NewBundle`:

```go
//go:embed *.toml
var localeFS embed.FS

func NewBundle() *locale.Bundle {
	b := i18n.NewBundle(language.English) // the fallback language
	b.RegisterUnmarshalFunc("toml", toml.Unmarshal)
	b.LoadMessageFileFS(localeFS, "active.en.toml")
	b.LoadMessageFileFS(localeFS, "active.zh-CN.toml")
	return locale.NewBundle(b)
}
```

```toml
[NoteNotFound]
other = "This note does not exist."

[NoteCount]
one = "{{.Count}} note"
other = "{{.Count}} notes"
```

Read the localizer with `locale.FromContext(ctx)` and handle the `false` case, which means the chain has no locale.
Message files use go-i18n syntax, including CLDR plural forms (`zero`, `one`, `two`, `few`, `many`, `other`; `other`
is required). A new language is a new `active.<tag>.toml` loaded into the bundle.

## Validate

The validate slot checks every proto request message against its protovalidate rules, so declaring a rule in proto is
all it takes; the rule syntax is protovalidate's own.

A failure returns 400 with reason `VALIDATOR`. The message is one sentence for the end user, chosen by the kind of the
first violation (length, range, format, required, other; an empty value that breaks a minimum counts as required),
without field names or rule text. Keel ships it in English, Chinese and Japanese so basic field checks need no
translations from you; the language is the first supported one in Accept-Language, ignoring q weights, falling back to
English. The full violation list stays in the error's cause and reaches the log. Sentinel passes `VALIDATOR` errors
through unchanged.

## Sentinel

Business code returns plain sentinel errors with no transport meaning; a `sentinel.Group` maps them to transport
errors at the edge, so the domain does not depend on HTTP or gRPC and the mapping lives in one place.

```go
var ErrNoteNotFound = errors.New("This note does not exist.") // in the domain

func NewNoteErrors() *sentinel.Group {
	return sentinel.NewGroup().
		On(biz.ErrNoteNotFound, sentinel.NotFound("NOTE_NOT_FOUND")).
		On(biz.ErrNoteLocked, sentinel.PreconditionFailed("NOTE_LOCKED")).
		OnLocaled(biz.ErrNoteTooLong, func(ctx context.Context, l *i18n.Localizer, err error) error {
			return errors.BadRequest("NOTE_TOO_LONG", l.MustLocalize(&i18n.LocalizeConfig{MessageID: "NoteTooLong"}))
		})
}
```

- Rules match with `errors.Is` in registration order, so a wrapped error (`fmt.Errorf("load note: %w", ErrNoteNotFound)`)
  matches too.
- `NotFound`, `Conflict`, `BadRequest` and `PreconditionFailed` (412 / `FAILED_PRECONDITION`: valid request, but the
  resource's state rules it out) send the sentinel's own text as the message, not the wrapped error's full text. The
  message reaches the end user as it is, so write the sentinel text as a sentence for them; context added with `%w` is
  for developers and stays in the log.
- `OnLocaled` hands the handler the request's localizer. With no locale on the chain it logs a warning and answers
  with `sentinel.InternalWithLog` (500), not a handler set with `Default`, so mount `WithLocale` when you use it.
- `Default(handler)` replaces the fallback for unmatched errors.

Unmatched errors, in order: a Kratos error carrying details (from a downstream service) passes through unchanged;
`context.Canceled` becomes 499 `CANCELED`; `context.DeadlineExceeded` becomes 504 `DEADLINE_EXCEEDED`; anything else
goes to the fallback, by default `sentinel.InternalWithLog`, which logs the error and returns 500 `INTERNAL` with the
message `internal error`.

Without sentinel nothing hides error text. `Default` does not include it and the server has no custom error encoder,
so Kratos returns an unrecognised error's full `err.Error()` as a 500 message, table names and all; a `%w` chain does
not stop that. Mount sentinel, or map every error to a Kratos error in the service layer.

Each error is logged once, where its type is converted; the sentinel fallback and the pass-through of downstream
errors follow that rule ([architecture](architecture.md)).
