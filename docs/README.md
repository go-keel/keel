# Using Keel

Read this to find the page for what you are about to do. Each page describes Keel from the outside: what to call, what
to declare, what Keel guarantees and what it refuses at startup, with the reasons. Signatures are in `go doc`.

- [quickstart.md](quickstart.md): bringing Keel into a project; the minimal `di.go` that runs a server.
- [architecture.md](architecture.md): deciding which layer code goes in, and where errors are translated and logged.
- [assembly.md](assembly.md): before writing a provider or changing `di.go`.
- [config.md](config.md): before adding a config key, loading your own config, or handling secrets.
- [observability.md](observability.md): before logging, adding spans, or configuring `logger` and `otel`.
- [errors.md](errors.md): when a `keel/<id>` error stops startup, or before matching one in code.
- [http.md](http.md): before adding an HTTP server, raw routes, SSE, WebSocket or downloads.
- [grpc.md](grpc.md): before adding a gRPC server or streaming methods.
- [middleware.md](middleware.md): before changing a middleware chain: slots, logging, locale, validation, the sentinel.
- [auth.md](auth.md): before turning on authentication or reading the caller's identity.
- [metadata.md](metadata.md): before passing metadata between services or using the client IP.
- [client.md](client.md): before calling another service over gRPC.
- [http-client.md](http-client.md): before calling another Keel service over HTTP (not for third-party APIs).
- [database.md](database.md): before using a database.
- [redis.md](redis.md): before using Redis, locks or rate limits.
- [messaging.md](messaging.md): before publishing or consuming events over NATS.
- [tasks.md](tasks.md): before enqueueing or handling background tasks.
- [worker.md](worker.md): before running a long-lived background process.
- [utilities.md](utilities.md): before using IDs, decimal conversion, JWT, passwords or JSON from `pkg/`.
- [proto.md](proto.md): before using Keel's proto options and shared messages.
