# Database

Read this before opening a database, mapping a database error, or turning on SQL logging.

`core/kdb` opens GORM databases on MySQL or PostgreSQL. What it does for you:

- A connection pool per named config section (max idle, max open, max lifetime).
- OpenTelemetry tracing and metrics at the `database/sql` layer (`otelsql`), from the global providers. Observability
  is a process facility set up by `kotel.Setup`, not a constructor parameter; when it is off the globals are harmless
  no-ops. Traces record statements with placeholders only; parameter values never reach a trace. Session resets,
  prepares and row iteration are not recorded (they cost storage and help no diagnosis), and the driver's
  `driver.ErrSkip` (MySQL without `interpolateParams` takes this step before preparing a statement with parameters)
  counts as neither a failure in traces nor in latency metrics.
- GORM's log goes through the standard `slog`, so SQL errors and slow queries share the pipeline and trace of the
  business logs.
- A `Ping` with a 5-second timeout at construction, so an unreachable database fails startup.
- Driver constraint errors translated into GORM sentinels (see "Driver errors").

## Configuration

```yaml
database:
  default:
    driver: "mysql"
    source: "user:password@tcp(localhost:3306)/notes?parseTime=True"
    max_idle: 10           # default 10
    max_open: 100          # default 100
    max_life_time: "300s"  # default 5 minutes; 0 means the default
    slow_threshold: "0.2s" # slow-query threshold; default 200 ms; 0 means the default
    log_level: "warn"      # silent | error | warn | info; default warn

  replica:
    driver: "mysql"
    source: "user:password@tcp(replica:3306)/notes?parseTime=True"
```

The defaults of `slow_threshold` and `log_level` are those of GORM's `logger.Default`. Negative durations and any
other `log_level` value fail startup.

## Dialects

`driver` is `mysql` or `postgres` (`pgx` is an alias of `postgres`). `kdb` imports no driver; package main imports the
dialect it uses, so a program on one database does not compile in the other's driver:

```go
import _ "github.com/go-keel/keel/core/kdb/mysql"    // driver: mysql
import _ "github.com/go-keel/keel/core/kdb/postgres" // driver: postgres / pgx
```

A configured dialect whose package is not imported makes `NewDB` fail before connecting, naming the import to add; any
other driver name fails config loading.

Keel is bound to GORM with MySQL or PostgreSQL and offers no hook for another dialect: an extension point for
swapping the stack could only expose what all candidates share, and what lies outside that is why the stack was chosen.
To use another database, call `gorm.Open` in your own code.

## Assembly

`kdb.NewDB(cfg)` opens `database.default`, `kdb.NewNamedDB(cfg, name)` any other section. Each returns a cleanup that
closes the pool; run cleanups in reverse order (rules in [Assembly](assembly.md), skeleton in
[Quickstart](quickstart.md)). With several databases, close the ones already opened when a later one fails:

```go
func NewData(cfg *kconfig.Config) (*Data, func(), error) {
    primary, primaryCleanup, err := kdb.NewNamedDB(cfg, "default")
    if err != nil {
        return nil, nil, err
    }
    replica, replicaCleanup, err := kdb.NewNamedDB(cfg, "replica")
    if err != nil {
        primaryCleanup()
        return nil, nil, err
    }
    return &Data{primary: primary, replica: replica}, func() { replicaCleanup(); primaryCleanup() }, nil
}
```

`kdb.DB` embeds `*gorm.DB`: call GORM directly on it (`First`, `Find`, `Create`, `Updates`, `Delete`,
`Transaction`). The embedding is also the raw handle. Read/write splitting is calling on a different `*kdb.DB`.

**Always pass `WithContext(ctx)` down the call chain.** Tracing, the trace link of slow-query logs and timeouts all
depend on it; forgetting it raises no error, they just silently stop working.

## Driver errors

`kdb` enables GORM's `TranslateError`, so the data layer matches GORM sentinels with `errors.Is`, without importing a
driver or knowing MySQL or PostgreSQL error codes:

| Sentinel | MySQL | PostgreSQL |
|---|---|---|
| `gorm.ErrDuplicatedKey` (primary or unique key) | 1062 | 23505 |
| `gorm.ErrForeignKeyViolated` | 1451, 1452 | 23503 |
| `gorm.ErrCheckConstraintViolated` | not translated | 23514 |
| `gorm.ErrInvalidField` (unknown column) | not translated | 42703 |

Other codes come back unchanged. The mapping is that of the `error_translator.go` files of the GORM MySQL and
PostgreSQL drivers at the versions in `go.mod`, applied in `DB.AddError`.

- **`Scan`, `Row` and `Rows` are not translated**, whether after a chained query or `Raw`: they assign the driver
  error to `db.Error` without `AddError`. `Create`, `First`, `Find`, `Updates`, `Delete`, `Exec` and `Raw(...).Find`
  are translated. Write anything you need to match by sentinel without `Scan`/`Row`/`Rows`; where you must use them,
  match the driver code and comment why the sentinel does not apply.
- **On MySQL you cannot tell which key was hit.** The MySQL driver returns the bare sentinel and drops the original
  error, so neither the error nor `kdb`'s log names the key or value. The PostgreSQL driver wraps the original with
  `%w`, so `errors.As` to `*pgconn.PgError` still gives the constraint name. On a table with several unique keys the
  caller has to know which one a write can hit: a write that can only collide on the title maps straight to "title
  already exists"; a write that can hit several keys queries first or is split. To log the collision, log the table
  and key value yourself.
- A sentinel is a decision point for the developer, not text for the user: map it to a business error in the data
  layer (see [Architecture](architecture.md) for where errors are mapped and logged). `gorm.ErrRecordNotFound` is
  mapped the same way.

## SQL logging

- SQL errors other than `gorm.ErrRecordNotFound` log at ERROR.
- A cancelled request context (`context.Canceled`, the caller went away) logs at DEBUG; `context.DeadlineExceeded`
  is still an error.
- A query slower than `slow_threshold` logs at WARN with `elapsed_ms`, `threshold_ms`, `rows` and `sql`. To stop
  logging slow queries, set `log_level` to `error` or `silent`.
- Other SQL is logged only with `log_level: info`.

Logs use the `*Context` slog variants, so they carry the request's trace.

There are two level gates. `database.<name>.log_level` is GORM's own level and decides which SQL events reach slog;
`logger.level` is the slog handler's gate and decides which of those are written. To see every SQL statement in
production temporarily, open both: `log_level: info` and `logger.level: info`. `db.Debug()` works as usual; it uses
GORM's `LogMode`.
