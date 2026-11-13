<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/keel-mark-reversed.svg">
  <img src="docs/assets/keel-mark.svg" alt="Keel" width="96">
</picture>

# Keel

[![Go Version](https://img.shields.io/badge/Go-1.27-blue.svg)](https://go.dev/)

A Go framework for gRPC and HTTP microservices, built on [Kratos](https://go-kratos.dev/) and designed for AI
coding agents first.

> The docs in this repository describe the main line. Read the [upgrade guide](UPGRADING.md) on every upgrade.

## What it gives you

- **Proto is the single source of the interface.** Services, HTTP mappings, request and response shapes, validation
  and error reasons are written once in `.proto`; server stubs, clients and OpenAPI are generated from it.
- **One way to assemble.** No DI tool: constructors return `(T)`, `(T, error)` or `(T, cleanup, error)`, and you wire
  them by hand in your own `di.go`.
- **Mistakes fail at startup.** Config that would be silently ignored, a misnamed slot, an annotation in the wrong
  place: Keel refuses to start and the error says which key or file is wrong, how to fix it, and where to read more.
- **A fixed stack, ready to use.** GORM with MySQL or PostgreSQL, Redis, NATS JetStream for events, asynq for tasks,
  a middleware chain with logging, tracing, metrics, validation and authentication.
- **Observability built in.** `log/slog` logs, OpenTelemetry traces and metrics.
- **Layer-friendly.** The mechanisms fit a service / biz / data split without enforcing one.

## Getting started

Add Keel to a Go module and bring up a server with the full middleware chain: [quickstart](docs/quickstart.md).
The [documentation index](docs/README.md) lists every topic with when to read it. Keel is developed by its
maintainer only; [AGENTS.md](AGENTS.md) says how.

## Links

- [Kratos documentation](https://go-kratos.dev/)
