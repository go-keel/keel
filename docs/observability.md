# Observability and logging

Read this before writing logs, adding spans or metrics, or configuring `logger` and `otel`.

Traces, metrics and logs come from OpenTelemetry, set up once by `kotel.Setup`; logs are written with the standard
library's `log/slog`, set up once by `keel.SetupLogger`. All three are **process facilities** (see
[GLOSSARY.md](../GLOSSARY.md)): installed globally at startup and used everywhere, never in the dependency graph, so
no constructor takes a logger, tracer or meter.

## Setup

```go
otelCleanup, err := kotel.Setup(cfg)
if err != nil {
    return err
}
cleanups = append(cleanups, otelCleanup)

keel.SetupLogger(cfg)
```

- `kotel.Setup` builds the tracer, meter and logger providers and registers them as the OpenTelemetry globals. It
  returns only a cleanup and an error: there is no provider object to hold. Components (`kdb`, `kredis`,
  `khttpclient`, `mw.Default`, `mw.ClientDefault`) read the globals themselves.
- With `otel.enabled: false`, `Setup` registers nothing and returns a no-op cleanup. The default OpenTelemetry globals
  are harmless no-ops, so `otel.Tracer(...)` still works and records nothing; never check for nil on the assembly
  path. The globals are either fully configured or fully no-op, never half.
- The globals hand over lazily: a tracer or logger taken before `Setup` is a placeholder that the real provider takes
  over in place. The order of providers in `di.go` relative to `Setup` does not matter.
- `keel.SetupLogger` calls `slog.SetDefault` and returns nothing, because the logger is not a graph node. It takes no
  observability argument: whether logs also go to OpenTelemetry depends only on `otel.enabled`.

## Config

```yaml
logger:
  level: info              # debug, info, warn, error (lowercase); default info, any other value fails startup
  stacktrace_level: error  # this level and above carry a stack trace; same values, default error
  console: true            # write to the console
  format: text             # console encoding: text (default) or json, one object per line once the logger is set up

otel:
  enabled: true
  endpoint: "otel-collector:4317"  # OTLP gRPC endpoint; required when enabled
  insecure: true                   # plain text, for development
  trace_sample_ratio: 1.0          # 0.0 to 1.0
  metric_interval: 10s
  log_batch_size: 512
  log_batch_interval: 1s
```

Where logs go:

- `logger.console: true` adds a console handler with source location and the trace IDs of the context, encoded as
  `format` says. `format` affects only the console; the OpenTelemetry side sends OTLP records. Lines written before
  `SetupLogger` runs (config loading, `maxprocs`) use the standard library's default text format.
- `otel.enabled: true` adds the OpenTelemetry handler, which reads the global logger provider.
- With neither, logs go to the stdout console, so they are never silently dropped.

`otel.endpoint` is required when `otel.enabled` is true; an empty one fails startup instead of falling back to the
SDK's default endpoint.

### Two deliberate choices

**An unreachable collector does not stop startup.** The OTLP gRPC exporter connects lazily, so a wrong endpoint or a
collector that is down lets the service start and telemetry silently fails to leave (the SDK retries and logs its own
errors). This is the opposite of `kdb` and `kredis`, which ping at startup and refuse to start without their server,
and the asymmetry is intended: without the database the service cannot work, so failing early helps; without the
collector the service works fully, so blocking startup would be backwards. When telemetry is missing, check the
collector and the network; the startup log will not say.

**What Keel sets comes only from the config file.** The service name and version (`app.name`, `app.version`), the
host name, the collector `endpoint` and `trace_sample_ratio` are passed to the OpenTelemetry SDK explicitly, so an
`OTEL_` variable cannot change them: Keel's resource wins over `OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES`, and
an explicit endpoint and sampler win over the SDK's environment defaults. Everything else the SDK reads from its own
`OTEL_` variables as upstream does: extra resource attributes from `OTEL_RESOURCE_ATTRIBUTES` are added next to
Keel's, and exporter settings such as `OTEL_EXPORTER_OTLP_HEADERS` and batch settings such as `OTEL_BSP_*` apply.

### Sampling

The sampler is parent-based: a span whose parent was sampled is sampled; a root span is sampled at
`trace_sample_ratio`. Use 1.0 in development and a fraction such as 0.1 in production.

`trace_sample_ratio: 0` (or absent) means "unset" and falls back to 1.0; to record no traces, set
`otel.enabled: false` instead. A value below 0 or above 1 fails startup rather than being clamped silently.

## Logging

Call the package-level `slog` functions everywhere; there is no Keel logger type and no logger to pass. Inside a
request use the context variants (`InfoContext`, `ErrorContext`, ...): the console handler adds `trace_id` and
`span_id` from the span in the context, and the OpenTelemetry handler reads the span context natively. Use the plain
variants only where there is no context, such as at startup.

```go
func (s *NoteService) CreateNote(ctx context.Context, req *pb.CreateNoteRequest) (*pb.Note, error) {
    note, err := s.uc.CreateNote(ctx, req.GetTitle(), req.GetBody())
    if err != nil {
        return nil, err // the layer that translated the error has logged it
    }
    slog.InfoContext(ctx, "note created", "note_id", note.ID)
    return toProtoNote(note), nil
}
```

```
time=2026-07-03T10:30:00.000+08:00 level=INFO source=service/note.go:25 msg="note created" trace_id=abc123def456 span_id=789abc note_id=12345
```

Arguments alternate key and value; any value is accepted and serialised by the handler. Levels: Debug for
diagnostics, usually off in production; Info for normal events; Warn for anomalies that need no action now; Error for
failures that need attention. Log each error once, where it is translated: see
[layered architecture](architecture.md).

## Request logs

Every HTTP and gRPC request is logged by the logging slot; fields, levels and body logging:
[middleware](middleware.md#logging).

## Tracing

Keel creates spans for:

| Operation | Span |
|---|---|
| HTTP and gRPC requests | the tracing middleware |
| database calls | `otelsql` at the `database/sql` layer, plus a per-query duration metric |
| Redis commands | the go-redis OpenTelemetry hook |
| publishing an event / enqueueing a task | `nats.publish` / `task.enqueue` |
| processing an event / a task | `nats.process` / `task.process`, continuing the trace from the message headers |
| calls to other services through `keel.NewServiceClient` | `mw.ClientDefault` propagates the trace |

Database and Redis instrumentation is active only with `otel.enabled: true`.

Events and tasks carry the trace context in their headers (NATS headers, asynq task headers), so a trace viewer shows
publisher and consumer as one trace. Both use OpenTelemetry semantic conventions v1.24.0: `messaging.system` (`nats`
or `asynq`), `messaging.destination.name` (the full subject, or the queue name), `messaging.operation` (`publish` or
`deliver`), plus `task.type` for tasks and `messaging.consumer` for event consumers. A failed processing attempt marks
the span as an error whether or not it will be retried; only success leaves the status unset.

For finer spans in your own code, take a tracer from the global; this is safe even before `kotel.Setup`:

```go
ctx, span := otel.Tracer("notes").Start(ctx, "IndexNote")
defer span.End()
if err := s.index(ctx, note); err != nil {
    span.RecordError(err)
    return err
}
```
