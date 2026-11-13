# Quickstart

Read this when bringing Keel into an existing project: the least you write after `go get` to run a server with the
full middleware chain.

## Requirements

- Go: the `go` line of Keel's `go.mod`.
- Only for the components you use: MySQL 8.0+ or PostgreSQL, Redis 7.0+, NATS 2.14+ with JetStream enabled.

## 1. Add the dependency

```bash
go get github.com/go-keel/keel
```

The optional modules (`core/kdb/mysql`, `core/kdb/postgres`, `core/kasynq`, `core/knats`, `core/khttpclient`) have no
tags of their own: fetch each one you use at the commit the core's tag points at, so the two match. A version such
as `@v0.1.0` does not work for them: Go looks for a tag that starts with the module's directory, such as
core/knats/v0.1.0.

```bash
v=$(go list -m -f '{{.Version}}' github.com/go-keel/keel)
go get github.com/go-keel/keel/core/kdb/mysql@$(git ls-remote https://github.com/go-keel/keel "refs/tags/$v^{}" | cut -f1)
```

Done when `go.mod` lists `github.com/go-keel/keel` and the optional modules you use.

## 2. Write the config file

`kconfig` reads `configs/config.yaml`, a fixed path relative to the working directory, so "which config did this
process read" never depends on flags. A `configs/config.dev.yaml` next to it is laid over it (for development, never
committed), and `KEEL_`-prefixed environment variables override both. The minimal config needs only `app` and `server`:

```yaml
app:
  name: "notes"
  version: "1.0.0"

server:
  http:                 # a map: name each server (default, public, admin, ...)
    default:
      addr: "0.0.0.0:8080"
      timeout: 30s

logger:
  level: "info"
  console: true

otel:
  enabled: false
```

`server.http`, `server.grpc` and the component sections (`database`, `redis`, `nats`, `asynq`, `client`) are maps:
configure as many named instances as you need and add them when you use them. Authentication is configured under
**each** server. Every field and constraint is in [config](config.md).

Done when the file exists at `configs/config.yaml` under the directory you run the process from.

## 3. Assemble and start

```go
package main

import (
    "log/slog"
    "os"

    "github.com/go-keel/keel"
    "github.com/go-keel/keel/core/kconfig"
    "github.com/go-keel/keel/core/kotel"
    "github.com/go-keel/keel/mw"
    "github.com/go-keel/keel/server/api"
)

func main() {
    if err := run(); err != nil {
        slog.Error("startup failed", "error", err)
        os.Exit(1)
    }
}

func run() error {
    var cleanups []func()
    defer func() {
        for i := len(cleanups) - 1; i >= 0; i-- {
            cleanups[i]()
        }
    }()

    cfg, err := kconfig.NewConfig()
    if err != nil {
        return err
    }

    // Tracing, metrics and logs are process facilities: Setup installs them as the
    // OpenTelemetry globals and returns nothing for anyone to hold.
    otelCleanup, err := kotel.Setup(cfg)
    if err != nil {
        return err
    }
    cleanups = append(cleanups, otelCleanup)

    // The logger is a process facility too: set the default once, then call package-level slog.
    keel.SetupLogger(cfg)

    chain := mw.Default(cfg.Config)

    srv, err := api.NewHTTPServer(
        cfg.GetServer().GetHttp()["default"],
        func(s *api.HTTPServer) {
            // pb.RegisterNoteServiceHTTPServer(s, svc)
        },
        chain,
    )
    if err != nil {
        return err
    }

    app, err := keel.NewAppFactory(keel.NewConfig(cfg)).New(srv)
    if err != nil {
        return err
    }
    return app.Run()
}
```

This sits in `main` to stay short; in a real project it goes in `cmd/<app>/di.go`. Keep exactly one assembly point,
because once construction is spread out nobody can tell how many times a resource was built.

That is the whole sequence: **config, observability, logger, middleware chain, server, App**. A database, Redis, a
message queue or a task queue adds one more `NewXxx(...)` in the middle and appends its cleanup to the slice; the order
and the rules are in [assembly](assembly.md).

`mw.Default` already holds recovery, tracing, metrics, logging, metadata and validate. Add locale, the sentinel and
rate limiting with the `mw.With*` presets ([middleware](middleware.md)); authentication is turned on in each server's
`auth` config ([auth](auth.md)).

Done when `go run ./cmd/<app>` starts and `curl localhost:8080/health` returns 200.

## Next

The index of every page, with when to read it, is [docs/README.md](README.md).
