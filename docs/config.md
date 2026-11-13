# Config

Read this before adding or changing a config key, loading your own service config, or deciding how secrets reach a
deployment.

Keel uses **two configs** that never mix: the framework config (`configs/config.yaml`, loaded by `kconfig.NewConfig`,
schema `conf/conf.proto`) and your service's own config (for example `configs/app.yaml`, loaded by
`kconfig.NewDynamic` into a type defined in your own proto).

## Files

```
configs/
├── app.yaml                 # your service config (schema: your own proto)
├── app.dev.yaml             # dev overlay, not committed, never in production
├── config.yaml              # framework config, committed; the file production deploys
├── config.dev.yaml          # dev overlay, not committed, never in production
└── config.dev.example.yaml  # example dev overlay, committed for reference
```

`config.yaml` and `app.yaml` are the **real main configs**: production deploys exactly these files. The committed
versions hold development placeholders, and the deployment mounts or injects the real files under the same names.
`config.dev.yaml` and `app.dev.yaml` are **dev overlays** for development only; production has neither. Loading one
logs the warning `kconfig: dev overlay loaded`, so that warning in a production log means a deployment step shipped
development config. To refuse to start instead, set `KEEL_ENV=prod` (see "Declaring production" below).

## Loading the framework config

`kconfig.NewConfig()` is the only entry point for the framework config. It:

1. reads `configs/config.yaml`, a **fixed** path relative to the working directory;
2. adds `configs/config.dev.yaml` as an overlay if it exists;
3. adds `KEEL_`-prefixed environment variables as the highest-priority source;
4. decodes the result into `conf.Config` and validates it (see "Validation").

Later sources override earlier ones: `config.yaml`, then `config.dev.yaml`, then `KEEL_` variables.

### The path is fixed

No flag or environment variable changes `configs/config.yaml`; mount the real config under `configs/` in the working
directory. Two reasons: the overlay name `config.dev.yaml` is fixed, and a renamable main file would drift from it
(main `prod.yaml`, overlay still `config.dev.yaml`); and with a variable path, "which config did this machine read"
could only be answered by inspecting the process environment.

### Environment overrides (`KEEL_` prefix)

Only `KEEL_`-prefixed variables take part, so unrelated variables such as `PATH` and `HOME` never become config keys.

**A variable never sets a config field by name.** The prefix is stripped and the rest kept as it is, so
`KEEL_JWT_SECRET` becomes the key `JWT_SECRET`: the decoder splits nested keys on `.`, which a shell variable name
cannot contain, so it cannot reach `server.http.default.auth.jwt_secret`, and no config field is named `JWT_SECRET`.

**Fields are overridden through placeholders.** Kratos config resolves `${KEY}` placeholders by
default, and Keel leaves it on. Write `${JWT_SECRET}` in the
nested field of `config.yaml` and set `KEEL_JWT_SECRET`: the placeholder is looked up by name (the part after `KEEL_`)
in the merged config, at any depth:

```yaml
server:
  http:
    default:
      auth:
        jwt_secret: "${JWT_SECRET}"   # ${KEY} or ${KEY:default}
```

```bash
KEEL_JWT_SECRET=the-real-secret ./notes
```

So: a field that differs per environment, secrets above all, gets a placeholder in
`config.yaml`, a literal local value in `config.dev.yaml` during development, and a `KEEL_XXX` variable injected by the
deployment in production. Neither path edits `config.yaml` per environment.

## Validation

Value constraints are declared on the fields of `conf/conf.proto` as protovalidate rules, and `NewConfig` checks them
once after loading. A failing config returns `keel/kconfig.invalid` and the process does not start; the message lists
every failing field by its config key (for example `app.node_id` or `redis["default"].addr`), not just the first.

| Section | Constraints |
|---|---|
| `app` | must exist; `name` required; `node_id` in `[-1, 1023]`; `stop_timeout` not negative |
| `logger` | `format` is empty, `text` or `json`; `level`, `stacktrace_level` are empty or lowercase `debug`, `info`, `warn`, `error` |
| `otel` | `endpoint` required when `enabled`; `trace_sample_ratio` in `[0, 1]`; `metric_interval`, `log_batch_size`, `log_batch_interval` not negative |
| `server.http` / `server.grpc` | `addr` required; `timeout` not negative |
| `server.*.auth` | `algorithm` is `RS256` (default) or `HS256`; with auth enabled, `HS256` needs `jwt_secret`, `RS256` needs `jwt_public_key_file` |
| `client` | `endpoint` required; `timeout` not negative |
| `database` | `driver` is `mysql`, `postgres` or `pgx`; `source` required; `max_open`, `max_idle`, `max_life_time`, `slow_threshold` not negative; `log_level` is empty or lowercase `silent`, `error`, `warn`, `info` |
| `redis` | at least one `addr`, none blank; `db`, `read_timeout`, `write_timeout` not negative |
| `nats` | `url` required; `stream_max_age`, `stream_replicas` not negative |
| `asynq` | `concurrency` not negative; queue names not blank, weights positive |

- **0 and an absent value mean "use the default".** The rules catch values an upstream library would silently replace
  with a default, clamp, or drop; the reason for each rule is in the comment on its field in `conf.proto`.
- **Rules judge values; components judge assembly.** A missing section (`database.<name>`, `asynq.<name>`), a provider
  that does not match the config (`app.node_id: -1` with the static node allocator), or `knats` without `app.domain`
  is still reported by the constructor that needs it.

- **Unknown keys are ignored, as in Kratos config**: a misspelled key is dropped without an error, and a section no
  code reads is not reported. Keel does not add either check; the constraints above cover the values that would
  otherwise be replaced silently.

A `kconfig.Config` built by hand (usually in a test) instead of through `NewConfig` is not validated; call `Validate`
on it when the test needs that.

## The framework config (config.yaml)

Top-level sections: `app`, `logger`, `otel`, `server`, and the maps `client`, `database`, `redis`, `nats`, `asynq`.

```yaml
app:
  domain: notes
  name: notes-api        # required
  version: 1.0.0
  node_id: 1             # [-1, 1023]; -1 means allocated from Redis
  stop_timeout: 20s      # whole-app shutdown budget, default 20s; every server's Stop(ctx) uses it

logger:
  level: info            # debug/info/warn/error; empty means info, any other value fails startup
  stacktrace_level: error  # this level and above carry a stack trace; default error
  console: false         # off in production: logs go through OpenTelemetry only
  format: text           # console encoding: text (default) or json

otel:
  enabled: true
  endpoint: "otel-collector:4317"
  insecure: true
  trace_sample_ratio: 1.0

server:
  http:
    default:             # a map: default / public / admin ...
      addr: "0.0.0.0:8080"
      timeout: 10s
      auth:              # each server has its own auth config
        enabled: true
        algorithm: RS256
        jwt_public_key_file: configs/rsa_public.pem
      client_ip:         # proxies trusted for the client IP; absent = loopback only
        trusted_proxies: ["10.0.0.0/8"]
    public:
      addr: "0.0.0.0:8081"
      timeout: 10s
      auth:
        enabled: false
  grpc:
    default:
      addr: "0.0.0.0:9090"
      timeout: 10s

database:
  default:
    driver: mysql
    source: "user:pass@tcp(db:3306)/notes?parseTime=True"
    max_idle: 10
    max_open: 100
    max_life_time: 300s
    slow_threshold: 0.2s
    log_level: warn

redis:
  default:
    addr:
      - "redis:6379"
    password: ""
    db: 0

nats:
  default:
    url: "nats://nats:4222"
    creds_file: ""       # nkey/JWT credentials file; empty means no authentication

asynq:
  default:               # worker behaviour only; which Redis it uses is chosen in di.go
    concurrency: 10
    queues:
      critical: 6
      default: 3
      low: 1

client:
  search-svc:
    endpoint: dns:///search-svc:9090
    timeout: 5s
```

How `client_ip` is used and why loopback is the default: [metadata](metadata.md).

`server.http` and `server.grpc` are maps, so one process can listen on several addresses (an external `public`, an
internal `admin`), each with its own `auth`. Pick one by key when assembling, such as
`cfg.Server.Http["admin"]`.

Constructors receive the framework config through assembly and read its fields directly
(`cfg.App.GetName()`, `cfg.Nats["default"].GetUrl()`).

## Dev overlay (config.dev.yaml)

`config.dev.yaml` sits next to the main config and is laid over it when present; it serves development only and is
neither committed nor deployed. Keep it in `.gitignore` (`configs/config.dev.yaml`).

```yaml
logger:
  level: debug
  console: true

otel:
  enabled: false

server:
  http:
    default:
      auth:
        enabled: true
        algorithm: HS256
        jwt_secret: "local-dev-secret"   # nested secret: write it here, never commit it

database:
  default:
    source: "root:password@tcp(localhost:3306)/notes?parseTime=True"
```

It keeps local settings in one readable file, needs no environment set up before each run, works when starting from an
IDE, and can set nested fields such as `jwt_secret` that environment variables cannot reach.

### Declaring production (KEEL_ENV)

`KEEL_ENV` declares the environment and accepts only `dev` and `prod`; unset means `dev`. Both `NewConfig` and
`NewDynamic` apply it when a dev overlay (`config.dev.yaml`, `app.dev.yaml`) is present; an unrecognised value fails
startup whether or not an overlay exists:

| `KEEL_ENV` | With a dev overlay in the working directory |
|---|---|
| unset or `dev` | the overlay applies and a Warn is logged |
| `prod` | startup fails with `keel/kconfig.dev-overlay-in-prod`, naming the file |
| anything else (`production`, `PROD`, ...) | startup fails with `keel/kconfig.env-invalid`: a typo must not pass as `dev`, or whoever set it believes they are protected when they are not |

Whether and where to set it is the deployment's choice; Keel does not require it.

## Your service config (app.yaml)

Settings specific to your service go in their own file, with a schema in your own proto, independent of Keel's
`Config`.

1. Define the schema:

   ```protobuf
   syntax = "proto3";

   package conf;

   option go_package = "notes/internal/conf;conf";

   message Bootstrap {
     Notes notes = 1;
   }

   message Notes {
     int32 max_note_bytes = 1;
     string search_api_key = 2;
   }
   ```

   ```yaml
   # configs/app.yaml
   notes:
     max_note_bytes: 65536
     search_api_key: "placeholder"
   ```

   Name the file after your config (`app.proto`), not `conf.proto`: Keel registers its own descriptor under the file
   path `conf.proto`, and the protobuf runtime panics at startup on a second file with that path. Generate `T` from the
   proto rather than writing a Go struct: the file is decoded as JSON, so a struct without JSON tags reads snake_case
   keys as zero values without an error.

   Done when the Go code is generated from the proto and compiles.

2. Load it with `kconfig.NewDynamic[T]`, which returns a handle that follows file changes:

   ```go
   bootstrap, cleanup, err := kconfig.NewDynamic[conf.Bootstrap]("configs/app.yaml")
   if err != nil {
       return nil, err
   }
   cleanups = append(cleanups, cleanup)
   ```

   Done when the process starts and logs no `kconfig` error.

3. Pass the handle to constructors and call `Get()` on every use, which always returns the latest value:

   ```go
   func NewNoteService(bootstrap *kconfig.Dynamic[conf.Bootstrap]) *NoteService {
       return &NoteService{bootstrap: bootstrap}
   }

   func (s *NoteService) maxBytes() int32 {
       return s.bootstrap.Get().GetNotes().GetMaxNoteBytes()
   }
   ```

What `NewDynamic` guarantees:

- **A missing file is an error**, never a silent zero-value config: a zero secret or a zero limit is much harder to
  track down than a failed start.
- **Constraints go on your own proto, with the same mechanism as the framework config.** When `T` is a proto message,
  its protovalidate rules are checked: at startup a failing config is returned as an error; on reload a failing config
  is rejected, the **last good config is kept**, and the Error log `kconfig: reload failed, keeping last good config`
  names the failing keys.
- **Reload survives watcher loss.** When the file watcher fails it is rebuilt with exponential backoff, so a
  Kubernetes-mounted ConfigMap (updated by swapping a symlink, which always breaks the watch on the old inode) keeps
  reloading.
- **Dev overlay by naming convention.** `<name>.dev.<ext>` next to the main file (`configs/app.yaml` →
  `configs/app.dev.yaml`) is laid over it and reloads with it.
- **Whether the overlay exists is fixed at startup.** An overlay created later is ignored until restart; otherwise
  editing the main file would apply it while editing the overlay alone would not, a half-applied state harder to debug
  than none.
- **Deleting the overlay while running makes reloads fail**: the last good config is kept and an Error is logged.

Merging follows Kratos config, the same for both entry points: **scalars and lists are replaced whole, maps merge key by
key, keys the overlay does not mention keep the base value.** Base `items: [a, b, c]` under overlay `items: [z]`
becomes `[z]`; base `m: {x: 1, y: 2}` under overlay `m: {x: 9}` becomes `{x: 9, y: 2}`.

Keep `app.dev.yaml` in `.gitignore` too. Like `config.dev.yaml` it logs `kconfig: dev overlay loaded` when loaded,
and **in a production log that warning is an incident signal**: `.gitignore` stops git, not a deployment step that
syncs the whole `configs/` directory, and `app.yaml` usually holds secrets. A process with `KEEL_ENV=prod` refuses to
start instead.

## Secrets

| Where | How |
|---|---|
| Local development | `config.dev.yaml`: plain text, not committed, can set nested secrets |
| CI/CD | the CI platform's secrets inject the config file or `KEEL_` variables for placeholders |
| Kubernetes | ConfigMap and Secret mounted as `configs/config.yaml` in the working directory |

Secrets such as `jwt_secret` are nested fields that environment variables cannot override directly: never commit
them. Use a `${KEY}` placeholder filled from a `KEEL_` variable, or let the deployment mount the real `config.yaml`.
