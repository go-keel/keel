# Metadata and the client IP

Read this before reading the client IP, trusting inbound metadata on an internal server, configuring trusted proxies,
or passing your own `x-keel-*` values between services.

The metadata slot of `mw.Default` reads request metadata into the context and `metadata.Client()` (in
`mw.ClientDefault`) passes every `x-keel-*` key on to the next service. Two keys are set by Keel: `x-keel-accept-language`
(the inbound value, else the Accept-Language header) and `x-keel-remote-address` (the client IP). Read them with
`metadata.GetAcceptLanguage(ctx)` and `metadata.GetRemoteIP(ctx)`, and the whole set with
`metadata.FromServerContext(ctx)`.

## Trust model

`x-keel-remote-address` exists to carry the original client IP across services: the entry service computes it from the
real request, `metadata.Client()` passes it down, and a downstream service reads the original client, not the address
of the sibling that called it. That only works if the value on the chain can be trusted, so:

| Server | Inbound `x-keel-remote-address` |
|---|---|
| default (every server) | ignored; computed from this hop's request and overwritten, or cleared when it cannot be computed |
| `mw.WithTrustedMetadata(chain)` | kept; computed only when the caller sent none |

Distrust is the default so an external caller cannot forge `GetRemoteIP()` by sending the header. Turn on trust only
on servers that provably receive no external traffic: internal gRPC services, or internal HTTP services behind a
trusted gateway. A server that can be reached from outside keeps the default.

```go
chain := mw.Default(cfg)
mw.WithTrustedMetadata(chain) // internal servers only
```

An internal server without trust overwrites the IP with what it sees (usually the calling sibling), so the chain of
original client IPs breaks there; that is the cost of the safe default.

The client IP is for logs and audit only. Do not use it for authentication or authorization.

## How the client IP is computed

Only two sources count: the connection peer and `X-Forwarded-For`. `X-Real-IP` and CDN headers (`CF-Connecting-IP`
and the like) are never read, nor is RFC 7239 `Forwarded`. Anyone can write `X-Forwarded-For`; only the entries
your own proxies (nginx, a load balancer) append can be trusted. So:

1. The peer is not a trusted proxy, or `ignore_forwarded_for` is set: the client IP is the peer; no header is read.
2. The peer is trusted: join every `X-Forwarded-For` line in order and walk from the right, skipping trusted entries.
   The first untrusted entry is the client IP; everything left of it was written by that client and can be forged.
   If that entry is malformed (`unknown`, empty, IPv6 with `%`), the proxies did not follow the convention, and the
   peer is used instead.
3. Every entry is trusted: take the leftmost. No `X-Forwarded-For`: use the peer.

Entries may carry ports (`1.2.3.4:5678`, `[2001:db8::1]:443`). The result is normalised, and IPv4-mapped IPv6
(`::ffff:1.2.3.4`) becomes `1.2.3.4`. With no peer (no transport, a unix socket) the result is empty and the key is
cleared.

Trusted proxies are configured per HTTP server, next to `auth`:

```yaml
server:
  http:
    default:
      client_ip:
        trusted_proxies: ["10.0.0.0/8", "127.0.0.1"] # addresses or masked ranges
        ignore_forwarded_for: false
```

- Without the section or without `trusted_proxies`, only loopback (`127.0.0.0/8`, `::1`) is trusted.
- `trusted_proxies` replaces the default rather than adding to it: list loopback again if you still need it.
- An entry that is not an address or a masked range fails config validation (`kconfig.invalid`), since dropping it
  would trust fewer proxies than written.
- `ignore_forwarded_for: true` trusts nobody and uses only the peer; setting it together with a non-empty
  `trusted_proxies` fails config validation.

gRPC servers take no such section and always use the default: the peer is the gRPC connection's peer, and only a
loopback peer has its inbound `x-forwarded-for` metadata read, by the same right-to-left walk. An HTTP server you build
yourself on Kratos with `metadata.Server` also uses the default.

Deployment convention: a proxy that connects from loopback or a listed address must overwrite or append
`X-Forwarded-For`. An nginx that only has `proxy_pass` passes the client's own header through, and its rightmost entry
is then whatever the client wrote; Keel cannot tell, so the fix is in the proxy's config.

- nginx on the same host: the default works, as long as nginx sets `X-Forwarded-For`.
- A proxy on another host or container: add its address to `trusted_proxies`.
- A local tool that forwards raw connections (socat, frp tcp, an SSH reverse tunnel) connects from loopback but writes
  no header: set `ignore_forwarded_for: true`.
- A CDN connecting directly: treat it as the open internet and do not list the CDN's ranges. They are shared by every
  customer of the CDN, and other people's edge functions connect from them too.

### Outside the chain

`GetRemoteIP` inside the chain reads `x-keel-remote-address`. Outside it, in a raw route or a `StreamContext.Filter`
filter, pass `r.Context()`: it returns the value `api.NewHTTPServer` computed before routing with this port's
`client_ip` config, and `WithTrustedMetadata` has no effect there. When neither exists (a server you built yourself)
it returns an empty string rather than recomputing, because recomputing would silently ignore the port's trusted
proxies. Always use `GetRemoteIP`; do not read `X-Forwarded-For` or `X-Real-IP` yourself.

## Your own x-keel-* keys

Only `remote-address` gets the rules above. Every other `x-keel-*` key, yours included, is accepted from the caller
and passed on as it is, so at the entry service it holds whatever the caller chose. External callers may send these
keys; the values just cannot be trusted. Stripping the prefix at the edge is optional hardening, not a requirement.

Values that must be trusted, such as a tenant or a user identity, are written by the entry service after
authentication and read downstream. Write them unconditionally, never only when missing: an injected value that
survives is carried by `metadata.Client()` to every later hop, where it cannot be told apart from one the entry wrote.

```go
md, _ := kmd.FromServerContext(ctx) // github.com/go-kratos/kratos/v3/metadata
md.Set("x-keel-notebook-id", notebookID) // set after authentication, always overwrite
```

```go
md, ok := metadata.FromServerContext(ctx)
if ok {
	notebookID := md.Get("x-keel-notebook-id")
	_ = notebookID
}
```

The HTTP client does not pass `x-keel-*` on: the prefix is a convention between services, and internal calls use
gRPC ([HTTP client](http-client.md)).
