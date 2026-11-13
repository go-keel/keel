# Service client

Read this before calling another service over gRPC.

`keel.NewServiceClient` builds a typed gRPC client for one service from the `client` section of the config and a client
middleware chain. It is the first choice for calling another Keel service: the proto contract, `x-keel-*` metadata
propagation and the circuit breaker are on this path. When the other side serves only HTTP, or the caller is outside
the service network, use the [HTTP client](http-client.md).

## Configuration

```yaml
client:
  notes:
    endpoint: "dns:///notes:9090" # direct (notes:9090) or a resolver URL
    timeout: 5s                   # default per-RPC timeout
```

`endpoint` is required at config load: the connection is lazy, so an empty one would fail only on the first call. A
service name with no section returns `client.config-missing`; a nil factory, empty name or nil constructor returns
`client.invalid-params`.

## Creating a client

```go
func NewNoteClient(factory *keel.ServiceClientFactory) (notesv1.NoteServiceClient, func(), error) {
	return keel.NewServiceClient(
		factory,
		"notes",                       // the key under client in the config
		notesv1.NewNoteServiceClient,  // the generated constructor
		nil,                           // nil: mw.ClientDefault
	)
}
```

The cleanup closes the connection; register it in `di.go` like any other cleanup ([assembly](assembly.md)).

The client is for calls inside the service network, to a gRPC server without auth. Kratos keeps gRPC's client-side
health checking on, and its `Health/Watch` stream carries no token; a server with auth rejects it, so the backend
counts as unhealthy and every call fails as unavailable. Call such a server through a second gRPC server of it that
has no auth, or over HTTP. Inject the
client into the repo that calls the service and use it as a normal gRPC client.

## The client chain

A nil chain means `mw.ClientDefault`: recovery → tracing → metrics → metadata (passes `x-keel-*` on) → circuit
breaker. It has no auth and no request logging. To send a token, build the chain and mount client auth; `token` is
called on every request:

```go
chain := mw.ClientDefault(factory.Config().Config)
mw.WithClientAuth(chain, func() string {
	return tokens.Current()
})
notes, cleanup, err := keel.NewServiceClient(factory, "notes", notesv1.NewNoteServiceClient, chain)
```

Your own client middleware goes in with `InsertAfter` / `InsertBefore` on a `mw.ClientSlot*` anchor
([middleware](middleware.md)). Chain errors are returned by `NewServiceClient`.

## Errors

When the called service follows the same rules, return its error unchanged: the source service already logged it, and
sentinel passes Kratos errors with details through ([middleware](middleware.md#sentinel)).

```go
note, err := r.notes.GetNote(ctx, &notesv1.GetNoteRequest{Id: id})
if err != nil {
	return nil, err // already logged by the notes service
}
```

A third-party service that does not follow these rules is logged and converted to a business error at this layer,
because each error is logged once, where its type is converted ([architecture](architecture.md)).
