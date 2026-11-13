# HTTP server

Read this before adding an HTTP server, an HTTP endpoint, a stream, a download or a webhook.

`api.NewHTTPServer` builds an HTTP server on Kratos `transport/http`. Every long-running part of a process (HTTP, gRPC,
consumers, tasks, workers) is a `keel.Server`; `factory.New(servers...)` runs them under one lifecycle, so they start
together and stop together on one SIGINT/SIGTERM. How to assemble them in `di.go` is in [assembly](assembly.md).

## Assembling a server

Each server reads one named section of `server.http` (a map: `default`, `public`, `admin`, ...), each with its own
`addr`, `timeout`, `auth` and `client_ip`. Give every server its own chain from `mw.Default`.

```go
httpCfg := cfg.GetServer().GetHttp()["public"]

chain := mw.Default(cfg)
mw.WithLocale(chain, bundle)
mw.WithSentinel(chain, noteErrors)

httpSrv, err := api.NewHTTPServer(
	httpCfg,
	server.RegisterHTTP(noteSvc),     // contract endpoints: the proto services
	chain,
	server.RegisterStreams(noteFeed), // raw routes (optional)
)
if err != nil {
	return err
}
app, err := factory.New(httpSrv, grpcSrv)
```

```go
func RegisterHTTP(notes *service.NoteService) api.HTTPServerRegister {
	return func(srv *api.HTTPServer) {
		notesv1.RegisterNoteServiceHTTPServer(srv, notes)
	}
}
```

```yaml
server:
  http:
    public:
      addr: "0.0.0.0:8000"
      timeout: 30s
      auth:
        enabled: true
        jwt_public_key_file: "configs/rsa_public.pem"
```

- Authentication is not wired here. The server builds it once from its own `auth` section and uses it both for the
  chain's auth slot and for `RequireLogin()` / `Anonymous()` on raw routes. See [auth](auth.md).
- A config read under a name that has no section is nil, and the constructor returns `api.http-config-missing`.
  Field rules such as a required `addr` are checked when the config loads ([config](config.md)).
- The server clones the chain before filling the auth slot, so one base chain can be passed to several servers.
- A plain-text port accepts both HTTP/1.1 and prior-knowledge HTTP/2 (h2c) on the same port; TLS negotiates through
  ALPN.
- At startup the server logs every proto route; raw routes are not listed.
- All servers stop within one deadline, `app.stop_timeout` (default 20s): after the signal, Keel calls every server's
  `Stop(ctx)` concurrently and waits for in-flight requests. Servers do not configure their own stop timeouts. A
  second signal during the stop exits the process at once with code 130, without waiting for the servers or running
  the cleanups.

## Two kinds of endpoints

**Contract endpoints** are declared in proto: Keel decodes and encodes them and every request runs the whole
middleware chain (auth, validate, logging, tracing, sentinel).

| Case | Mechanism | Encoding |
|---|---|---|
| 1a request/response | unary RPC with `google.api.http` | protojson |
| 1b server push (SSE) | server-streaming RPC | protojson per frame |
| 1c bidirectional stream (WebSocket) | bidi-streaming RPC | protojson per frame |
| 1d binary response | unary RPC returning `google.api.HttpBody` | raw bytes and `content_type` |

**Raw routes** are plain `http.Handler`s registered through the stream register. They skip the middleware chain and
own their encoding.

| Case | Tool |
|---|---|
| 2a free-form SSE | `api.SSEHandler` |
| 2b free-form WebSocket | `api.WebSocketHandler` |
| 2c file download with Range | `api.ServeContent`, `api.ServeReader` |
| 2d raw request body (webhook signatures) | `sc.Server().HandleFunc` |
| 2e a filter over the whole port | `sc.Filter` |

Choose a contract endpoint whenever proto can express it: types, validation, OpenAPI, both gRPC and HTTP, and the full
chain come for free. Use a raw route only when you need what the chain hides: raw bytes, free-form frames, Range, your
own encoding. Each raw case mirrors a contract case (1a↔2d, 1b↔2a, 1c↔2b, 1d↔2c).

### 1a: unary RPC

Declare the route in proto with `google.api.http`; generation writes `RegisterXxxHTTPServer` and the request binding.

```protobuf
service NoteService {
  // {id} binds from the path.
  rpc GetNote(GetNoteRequest) returns (Note) {
    option (google.api.http) = { get: "/v1/notes/{id}" };
  }
  // body "*" maps the whole request body.
  rpc CreateNote(CreateNoteRequest) returns (Note) {
    option (google.api.http) = { post: "/v1/notes", body: "*" };
  }
  // Fields not in the path come from the query string.
  rpc ListNotes(ListNotesRequest) returns (ListNotesResponse) {
    option (google.api.http) = { get: "/v1/notes" };
  }
}
```

Endpoints that need no credential are marked in proto with `option (keel.v1.auth) = { allow_anonymous: true };`, see
[auth](auth.md).

### 1b: server-streaming RPC as SSE

The generated HTTP handler serves a server-streaming RPC as `text/event-stream`, one `data:` frame per message. One
implementation serves both gRPC streaming and HTTP, and the stream runs the same chain as a unary call, once per stream
(see [gRPC](grpc.md)), so the anonymous annotation works here too.

```protobuf
rpc WatchNotes(WatchNotesRequest) returns (stream Note) {
  option (google.api.http) = { get: "/v1/notes/watch" };
}
```

```go
func (s *NoteService) WatchNotes(req *v1.WatchNotesRequest, stream v1.NoteService_WatchNotesServer) error {
	for _, n := range snapshot {
		if err := stream.Send(toProtoNote(n)); err != nil {
			return err
		}
	}
	// Returning ends the stream. To keep pushing, loop and return when Send fails: over HTTP that is the only
	// signal that the client left (stream.Context() is cancelled only on gRPC).
	return nil
}
```

Frames are negotiated by `Accept`; with none or `application/json` they are protojson, the same as unary responses.
Use 1b when messages have a contract shape and need auth and logging; free-form text (log tails, progress lines) is 2a.

### 1c: bidi-streaming RPC as WebSocket

A bidi-streaming RPC with a `google.api.http` GET maps to WebSocket; chain and frame encoding follow 1b.

```go
func (s *NoteService) LookupNotes(stream v1.NoteService_LookupNotesServer) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil // the client closed its send side
		}
		if err != nil {
			return err
		}
		n, err := s.uc.GetNote(stream.Context(), req.GetId())
		if err != nil {
			return err
		}
		if err := stream.Send(toProtoNote(n)); err != nil {
			return err
		}
	}
}
```

The generated handler upgrades the connection before the chain runs. An auth failure therefore arrives after a
successful 101 handshake, as a text frame starting `\x1eerror:` with the error text followed by close code 1011; the
client never sees a 401, so it must handle an error in the first frame. An error the handler returns later in the stream
ends it the same way: the frames already sent arrive, then one such `\x1eerror:` frame carrying the code and reason,
then close code 1011. SSE does not have this problem: headers are written with the first frame, so a middleware error is
still a normal HTTP status. An error in the middle of an SSE stream is sent as an `event: error` frame, because the
status is already sent.

Use 1c for streams of contract messages; free-form frames (text protocols, binary relays, third-party formats) are 2b.

### 1d: binary response through HttpBody

A unary RPC that returns `google.api.HttpBody` writes `data` as the body with `content_type` as the Content-Type and
still runs the chain. It suits small exports; large files that need Range are 2c.

```go
func (s *NoteService) ExportNotes(ctx context.Context, req *v1.ExportNotesRequest) (*httpbody.HttpBody, error) {
	csv, err := s.uc.ExportCSV(ctx)
	if err != nil {
		return nil, err
	}
	return &httpbody.HttpBody{ContentType: "text/csv", Data: csv}, nil
}
```

### Internal endpoints stay in proto

Operational endpoints you do not want in the public contract still belong in proto: put the service in its own proto
file and exclude that directory from OpenAPI generation. It keeps types, validation, the chain and the anonymous
annotation, and simply does not appear in the published spec.

## Liveness: every HTTP server has /health

`api.NewHTTPServer` registers `GET /health` (and `HEAD`; other methods get 405) before any of your routes. It answers
200 with the body `ok`. Do not register it yourself.

- It means liveness only: the process is up and answering. It does not probe the database, Redis or NATS, because load
  balancers probe every instance and one dependency blip would take all instances out at once; dependency failures
  show up in logs and alerts.
- It skips the middleware chain and needs no credential, so it works on servers with auth. It produces no request log,
  span or metric.
- It is registered first, so a prefix route at the root (such as a single-page app) cannot swallow it.
- A proto route or a `RequireLogin()` / `Anonymous()` raw route (`Handle` / `HandleFunc`) on `/health` fails startup
  with `api.health-route-conflict` instead of being silently hidden. A route mounted through the `Server()` escape hatch
  cannot be detected and is shadowed, so do not mount one there.

Processes that only consume events, run tasks or run cron have no HTTP server and no `/health`; the process manager
watches the process instead.

## Raw routes

Register raw routes with a `StreamRegister` passed as the last arguments of `NewHTTPServer`. Every registration
declares its authentication: `sc.RequireLogin()` (a valid credential is required) or `sc.Anonymous()` (no credential
required; a valid one is still recorded). There is no undeclared form, so a missing declaration is a compile error
rather than a route silently open to anonymous callers.

- Both read the token like the contract endpoints: `Authorization: Bearer` first, then the cookie named by
  `auth.cookie_name` if configured. Browsers cannot set headers on `<img>`, EventSource, WebSocket handshakes or
  download links, so cookie sessions depend on that fallback.
- A rejection uses the same structured error as contract endpoints, distinguishing a missing credential from an invalid
  one. On success the claims are in the request context (`auth.FromContext(r.Context())`).
- On a server without auth, `Anonymous()` passes everything through and `RequireLogin()` fails startup
  (`api.require-login-without-auth`): with no verifier, the declaration cannot hold.
- Raw routes are registered after the proto routes, so on a path a contract endpoint already uses, the contract
  endpoint wins and the raw route never runs. Give raw routes paths no contract endpoint uses.
- `Handle` / `HandleFunc` / `HandlePrefix` use the server's own router, and Keel gives no accessor for `{id}` path
  variables on raw routes; match a prefix and parse the rest of the path yourself.
- `sc.Server()` is the escape hatch: routes mounted on it skip Keel's authentication entirely. Use it only when the
  route authenticates itself (2d), and say so in a comment.

### 2a: free-form SSE

```go
func RegisterStreams(feed *biz.NoteFeed) api.StreamRegister {
	return func(sc *api.StreamContext) {
		sc.RequireLogin().HandleFunc("/v1/notes/events", api.SSEHandler(func(ctx context.Context, w *api.SSEWriter) error {
			sub, err := feed.Subscribe(ctx)
			if err != nil {
				return err
			}
			for {
				select {
				case <-ctx.Done():
					return nil // the client went away
				case msg, ok := <-sub:
					if !ok {
						return nil
					}
					if err := w.WriteEvent(api.SSEEvent{Event: "message", Data: msg}); err != nil {
						return err
					}
				}
			}
		}))
	}
}
```

`SSEHandler` sets the event-stream, no-cache and `X-Accel-Buffering: no` headers and flushes after every event; it ends
when the source returns or the client disconnects. A writer that cannot flush is refused (`api.sse-no-flush`).

### 2b: free-form WebSocket

```go
sc.RequireLogin().HandleFunc("/ws/notes", api.WebSocketHandler(
	func(ctx context.Context, conn *websocket.Conn) error {
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return err // a client disconnect counts as a normal end
			}
			if err := conn.WriteMessage(mt, data); err != nil {
				return err
			}
		}
	},
	api.WSOptions{ReadLimit: 1 << 20}, // per-frame limit; give CheckOrigin an allowlist for cross-origin use
))
```

A normal return sends `CloseNormalClosure`; an error sends `CloseInternalServerErr` and the details go only to the log.
Client closes (1000, 1001, 1005, 1006) count as normal ends and are not logged as errors. A frame larger than
`ReadLimit` closes the connection with 1009 (message too big) and logs a warning, not an error: it is the client's
doing.

### 2c: file download

```go
sc.RequireLogin().HandlePrefix("/v1/files/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/files/")
	f, err := storage.Open(r.Context(), id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	api.ServeContent(w, r, f, api.DownloadOptions{Filename: f.Name(), ContentType: "application/pdf"})
}))
```

`ServeContent` takes an `io.ReadSeeker` and supports Range and conditional GET; `ServeReader` streams an `io.Reader`
without Range. The file name is escaped for quotes and non-ASCII names (RFC 6266/5987).

### 2d: raw request body

A callback whose signature covers the raw body (the signature is the credential, and the body cannot be rebuilt once
decoded) goes on the `sc.Server()` escape hatch. The route skips Keel's authentication by explicit choice.

```go
sc.Server().HandleFunc("/webhooks/notes", func(w http.ResponseWriter, r *http.Request) {
	// MaxBytesReader, not io.LimitReader: an oversized body is an error, not silently truncated and then verified.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "request body too large or unreadable", http.StatusBadRequest)
		return
	}
	if !verifyHMAC(body, r.Header.Get("X-Signature"), secret) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusOK)
})
```

A callback with a fixed shape that does not need the raw bytes is better as 1a.

### 2e: a filter over the whole port

When a check must cover every endpoint of the port, contract endpoints included (they decode the body before the
chain runs), mount a pre-routing filter with `sc.Filter`. Keel fixes the order: compute the client IP → filters (first
mounted is outermost) → router. Inside a filter, `metadata.GetRemoteIP(r.Context())` already returns this port's client
IP. A filter sees every request, `/health` included, so pass through what it does not own; the middleware chain
(auth, logging, tracing, recovery, error mapping) has not run yet. A nil filter fails startup (`api.stream-filter-nil`).

Do not wrap the server's `Handler` after `NewHTTPServer` returns: that wrapper runs before the client IP is computed,
and `GetRemoteIP` returns an empty string there.

## A query value that cannot be decoded

Query parameters are decoded before validation runs. A value that cannot become the field's type (an enum number or
name the enum does not define, text in a number field) is rejected at decoding with 400 and reason `CODEC`, and its
message is the decoder's own English text; protovalidate rules never see it. Over gRPC the same request is decoded
from the binary message and reaches validation, so the reason differs between the two transports.

## JSON encoding is protojson

`server/api` installs the official Kratos contrib JSON codec, which encodes `proto.Message` values with protojson
(`EmitUnpopulated`, `DiscardUnknown`) and everything else with the standard library. Unary responses, error responses
and stream frames all use it, so they agree:

- `Timestamp` is an RFC 3339 string.
- Enums are names, not numbers.
- `int64` is a string, so JavaScript does not lose precision.
- Unset fields are still present.

Browsers and `fetch`/axios get this without any `Accept` header; a caller that negotiates another format (such as
`application/proto`) still gets it. Request bodies with `Content-Type: application/json` are parsed with protojson.
A process can register only one "json" codec, so do not register your own.

## Error responses

Errors use the Kratos error shape:

```json
{"code": 404, "reason": "NOTE_NOT_FOUND", "message": "This note does not exist.", "metadata": {}}
```

The message is a sentence the end user reads. Mount sentinel ([middleware](middleware.md#sentinel)) to map business
errors and to keep unknown errors from leaking their text; it also maps `context.Canceled` to 499 and
`DeadlineExceeded` to 504.
