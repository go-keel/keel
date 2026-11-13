# gRPC server

Read this before adding a gRPC server or a streaming RPC, or when a gRPC health probe is rejected.

`api.NewGRPCServer` builds a gRPC server on Kratos `transport/grpc`. It takes one named section of `server.grpc`, a
register function and a middleware chain, and returns a `keel.Server` for `factory.New(...)`. A process that serves
HTTP outside and gRPC inside builds both and passes both to `factory.New(httpSrv, grpcSrv)`.

```go
func RegisterGRPC(notes *service.NoteService) api.GRPCServerRegister {
	return func(srv *api.GRPCServer) {
		notesv1.RegisterNoteServiceServer(srv, notes)
	}
}

grpcCfg := cfg.GetServer().GetGrpc()["default"]
grpcSrv, err := api.NewGRPCServer(grpcCfg, server.RegisterGRPC(noteSvc), mw.Default(cfg))
if err != nil {
	return err
}
```

```yaml
server:
  grpc:
    default:
      addr: "0.0.0.0:9000"
      timeout: 30s
      auth:
        enabled: true
        jwt_public_key_file: "configs/rsa_public.pem"
```

- Authentication comes from the server's own `auth` section and fills the chain's auth slot, as on HTTP; see
  [auth](auth.md). A missing section returns `api.grpc-config-missing`.
- The chain and its presets are shared with HTTP ([middleware](middleware.md)). gRPC takes no `client_ip` section:
  it always uses the default trust rules ([metadata](metadata.md)).
- A gRPC-only RPC needs no `google.api.http` annotation.

## Streams run the chain once per stream

Unary RPCs run the chain once per call. A streaming RPC runs it once when the stream opens, and the chain wraps the
whole life of the stream, the same as SSE and WebSocket on HTTP. So auth rejects at stream open, the
tracing span covers the whole stream, the metrics duration is the connection time, and logging writes one completion
record per stream.

The chain does not run per message. Kratos's `StreamMiddleware` fires on every message, which would count messages as
requests and open a span per message, so Keel does not use it.

Two differences from unary that handlers must handle:

- Stream messages are not validated. The handler receives them with `RecvMsg`; at stream open the interceptor does not
  know their type. Validate inside the handler when you need it.
- There is no `phase=open` log at stream start; that log exists only for HTTP SSE and WebSocket. Long gRPC streams are
  observed through tracing.

Rate limiting and long-lived streams: [middleware](middleware.md#building-a-chain).

## Health checks

Do not register a health service: Kratos registers `grpc.health.v1.Health` on every gRPC server, SERVING after `Start`
and NOT_SERVING after `Stop`. Registering it again kills the process at startup with a gRPC duplicate-registration
fatal error, not a returned error.

The built-in health server is private to Kratos, so per-service status (`SetServingStatus`) has no way out through
Keel; the built-in status covers "the process is up and serving".

On a server with auth, probes are rejected: `Health/Check` and `Health/Watch` run the chain, and the anonymous
annotation cannot be added to them because their descriptors live in grpc-go. Choose one: give the probe a token,
probe the `/health` of an HTTP server in the same process (no chain, no credential, see
[HTTP](http.md#liveness-every-http-server-has-health)), or expose health on a second gRPC server whose config has no
auth.

Server reflection is on, as in Kratos (`grpcurl list` works without proto files). On a server with auth it runs the
chain like any other call, so listing needs a token.

## Errors and tracing

Kratos errors map to gRPC status codes (`errors.NotFound` → `NOT_FOUND`, `errors.BadRequest` → `INVALID_ARGUMENT`).
The tracing middleware creates the server span and propagates context on clients; log with `slog.InfoContext(ctx, ...)`
and the record carries the trace. To call other services, use `keel.NewServiceClient` ([client](client.md)).
