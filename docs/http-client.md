# HTTP client

Read this before calling another Keel service over HTTP.

`core/khttpclient` (an optional module) is an HTTP client for calling services built on Keel, on Resty v2. It restores
Keel's error envelope, logs every request, carries traces and metrics, and speaks HTTP/2 including plain-text h2c. It
is not a general client for third-party APIs.

## Which path

To call another Keel service, in this order:

1. gRPC through the [service client](client.md): the proto contract, `x-keel-*` propagation and the circuit breaker
   are all there.
2. This client with the other service's generated proto types, when it serves only HTTP or the caller is outside the
   service network: decode with `SetResult(khttpclient.ProtoResult(&msg))`. Do not pass a generated type straight to
   `SetResult`: that uses `encoding/json`, which cannot read quoted int64s, RFC 3339 Timestamps or enum names. Do not read
   `resp.Body()` and call `protojson.Unmarshal` yourself either: it is easy to forget `DiscardUnknown`, so the first field
   the other side adds breaks the call, and the client's uniform decode errors are skipped.
3. This client with your own structs, when you depend only on the HTTP+JSON contract and not on the other service's
   proto package. Tag fields per "Response field shapes" below.

## Usage

The client reads no config: you pass the endpoint and cache the client yourself; call the cleanup when the application
stops. It takes no observability arguments, because traces and metrics always use the global OpenTelemetry providers
([observability](observability.md)).

```go
cli, cleanup, err := khttpclient.New(
	"http://notes:8000",
	khttpclient.WithH2C(),              // plain-text HTTP/2 inside the network
	khttpclient.WithServiceName("notes"), // log field and span peer.service
)
```

```go
var out struct {
	Title     string `json:"title"`
	CreatedAt string `json:"createdAt"` // Timestamp arrives as an RFC 3339 string
}
_, err = cli.R().SetContext(ctx).SetResult(&out).Get("/v1/notes/1")
```

```go
var note notesv1.Note
_, err = cli.R().SetContext(ctx).SetResult(khttpclient.ProtoResult(&note)).Get("/v1/notes/1")
// read note itself; resp.Result() returns an internal wrapper
```

`ProtoResult` decodes with protojson, matching the server's output, and always discards unknown fields: the other side
adding fields is normal and the caller should not fail before it does. It applies to that one request; a plain struct
on the same client still uses `encoding/json`. A nil or typed-nil target is an error (`khttpclient.nil-result`).
protojson is far stricter than `encoding/json` (a Timestamp of `""` fails), and after a decode failure the target may be
cleared or half written, so do not use it. Request bodies go through `SetBody` as plain JSON; the server decodes them
leniently.

Options: `WithH2C()` (only for `http://`), `WithTimeout(d)` (whole request, default 5s, 0 for none), `WithTLSConfig(c)`,
`WithServiceName(name)` (defaults to the endpoint host), `WithResponseBodyLimit(n)` (default 2 MB, positive only).

## Errors

```go
if khttpclient.IsReason(err, "NOTE_NOT_FOUND") {
	return nil, biz.ErrNoteNotFound
}
if info, ok := khttpclient.AsError(err); ok {
	slog.WarnContext(ctx, "call notes failed", "code", info.Code, "reason", info.Reason)
}
```

A non-2xx response with the error envelope becomes an error you check by reason string, without importing the other
side's enums or touching Kratos errors. Branch only on `Reason`: `Message` may change with the language, `Code` is the
real status, `Metadata` may be nil.

`Reason` is empty in three cases, and `IsReason(err, "")` is always false because Keel does not invent a reason:

| Case | `Code` | `Message` |
|---|---|---|
| transport failure (dial, timeout; no response) | 500 | the underlying error |
| non-2xx without an envelope (a gateway's HTML page) | the real status | the status text |
| 2xx that breaks the contract (decode failure, refused content type) | the real 2xx | why it was refused |

The last case keeps "it answered 200 in the wrong shape" apart from "it failed" and "it never answered". A 2xx `Code`
means the other side succeeded and only the response could not be read: do not retry a non-idempotent call on it; check
the result first. This error is not a Kratos `*errors.Error`; returned as it is from your own server, Kratos writes a
500, whereas carrying the 2xx along would send the failure as a success.

## Response content types

Only two kinds of 2xx response with a body are accepted: `application/json` and `*+json`, decoded by `SetResult`; and
`text/*`, read with `resp.String()`. Anything else (binary, images, `application/octet-stream`) is a call error. Bodiless
responses such as 204 pass, and non-2xx responses go to envelope handling, so an HTML error page still degrades to its
status. Requests send `Accept: application/json, text/*;q=0.9` to match, so a negotiating server does not answer a text
endpoint with 406.

Refusing binary is about what the client is for: under a 2 MB limit a large file is cut off and a small one is buffered
whole, and neither is an API call. Downloads and streams use their own channels.

`SetResult` on a text response is an error rather than a silently zero target, the hardest kind of failure to find;
read text with `resp.String()`, which works for JSON too and decodes nothing.

## Response field shapes

For hand-written structs, Keel servers write protojson:

| Proto | On the wire | Model as |
|---|---|---|
| snake_case field | camelCase (`node_id` → `nodeId`) | camelCase json tag |
| int64 / uint64 | string (`"42"`) | `string` or `json.Number` |
| Timestamp | RFC 3339 string | `string` or `time.Time` |
| enum | name (`"ACTIVE"`) | `string` |
| unset field | present, zero value | accept zero values |

## Logging

Each request writes exactly one slog record with `service`, `method`, `url`, `status`, `latency`: 2xx is INFO, any other
status (3xx included) is WARN, and a failure not covered by those is ERROR with `error` (and `status` when a response
arrived). `url` is the full URL with the query, as it is; the client redacts nothing. Bodies and headers are not logged;
log them yourself when needed.

## Limits and escape hatches

- `New` returns a native `*resty.Client`; turn on retries and other Resty features yourself.
- `x-keel-*` metadata is not passed on, by design: the prefix is a convention between services and internal calls use
  gRPC. This client does not assume it is inside the trust boundary; set the headers you need explicitly
  ([metadata](metadata.md)).
- Decoding runs in the response hooks, not in Resty's own parser, which runs before every hook and would skip them on
  failure; `New` turns that parser off. Calling `SetJSONUnmarshaler` hands decoding back to Resty, and such failures
  fall back to `Code` 500 with no `status` in the log.
- The 2 MB body limit keeps one runaway upstream from exhausting the process; an API response that large should have
  been paginated. `WithResponseBodyLimit` accepts only positive values, so assembly has no way to turn it off, but
  `cli.SetResponseBodyLimit(0)` on the native client can, and the memory risk is then yours.
- Streams (WebSocket, SSE), large responses and downloads are out of scope. Resty's `SetDoNotParseResponse` and
  `SetOutput` work technically, but skip the response hooks: no logging, no envelope restoration, and you close the body.
- Service discovery is not supported; the endpoint is a fixed address.
