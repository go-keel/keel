# Authentication

Read this before turning on JWT authentication for a server, marking an endpoint anonymous, reading the caller's claims,
or sending a token to another service.

Each server owns its authentication. `api.NewHTTPServer` / `api.NewGRPCServer` build it once from the
server's own `auth` section and use it both for the chain's auth slot (contract endpoints) and for
`RequireLogin()` / `Anonymous()` (raw routes), so the two paths behave the same rule for rule and return the same
error. The `middleware/auth` package only exposes the result: reading and writing claims in the context, the client
middleware, and the error values. There is no separate verifier or middleware to assemble.

## Configuration

Pass a `mw.Default` chain to the server and configure `auth` per server; nothing else is wired.

```yaml
server:
  http:
    default:
      addr: "0.0.0.0:8000"
      auth:
        enabled: true
        algorithm: "RS256"                          # the default when omitted
        jwt_public_key_file: "configs/rsa_public.pem"
```

```yaml
      auth:
        enabled: true
        algorithm: "HS256"
        jwt_secret: "at-least-32-bytes-of-secret-key!"
```

- RS256 loads the public key from `jwt_public_key_file`; HS256 needs `jwt_secret`. Another algorithm fails config
  validation (`kconfig.invalid`).
- Prefer RS256 between services: the public key can be handed out and the private key stays with the issuer, such as
  a gateway that signs and internal services that only verify. HS256 suits a single application.
- Verification checks the signature, requires `exp`, and pins the algorithm, which blocks algorithm-confusion attacks.
- Never commit `jwt_secret`: keep it in the dev overlay for local work and let deployment supply it in production
  ([config](config.md)).

Startup fails instead of running open in two cases: auth enabled on a server whose chain is `mw.Minimal()`
(`mw.no-auth-slot`), and a `RequireLogin()` raw route on a server without auth (`api.require-login-without-auth`).

### Token from a cookie

By default only `Authorization: Bearer` is read. A browser-facing server can set `cookie_name` to fall back to that
cookie when the header is missing; a token in an `HttpOnly` cookie cannot be read by script, so an XSS cannot steal it.

```yaml
      auth:
        enabled: true
        cookie_name: "notes_session" # empty: header only
```

- The header wins over the cookie: an explicit caller (another service, curl, a narrowly scoped one-off token) should
  not be overridden by a cookie the browser attached.
- CSRF is the issuer's job: browsers attach cookies to cross-site requests, so issue the cookie with `SameSite=Strict`
  or `Lax`. Keel only reads the token.
- Cookies exist only on HTTP; the setting has no effect on gRPC.

## Anonymous endpoints

Mark a contract endpoint anonymous in proto:

```protobuf
import "keel/v1/annotations.proto";

service SessionService {
  rpc SignIn(SignInRequest) returns (SignInResponse) {
    option (keel.v1.auth) = { allow_anonymous: true };
  }
}
```

At startup Keel scans every loaded proto file for the annotation. An anonymous endpoint lets requests through with a
missing or invalid token; a valid token still puts its claims in the context. Any other endpoint answers a missing token
with `not signed in` and an invalid one with `invalid token`, both 401 with reason `UNAUTHORIZED`, and never says why
verification failed.

The annotation is read from the method's own options, so it works only on contract endpoints. Raw routes declare
`RequireLogin()` or `Anonymous()` instead, and `sc.Server()` routes skip Keel's auth by explicit choice; see
[HTTP raw routes](http.md#raw-routes). gRPC health probes cannot carry the annotation ([gRPC](grpc.md#health-checks)).

## Reading the caller

```go
func (s *NoteService) ListMyNotes(ctx context.Context, req *v1.ListMyNotesRequest) (*v1.ListMyNotesResponse, error) {
	claims, ok := auth.FromContext(ctx)
	if !ok {
		return nil, errors.Unauthorized("UNAUTHORIZED", "Please sign in first.")
	}
	owner, _ := claims["sub"].(string)
	return s.uc.ListByOwner(ctx, owner)
}
```

`ok` is false on anonymous endpoints called without a valid token. Common claims are `sub` (user ID), `exp`, `iat`
and `iss`. Authentication answers who the caller is; checks such as bans or permissions belong in your own middleware
inserted after the auth slot ([middleware](middleware.md#adding-your-own-middleware)).

`ErrMissingJwtToken`, `ErrInvalidToken` and `ErrWrongContext` share code and reason, so `errors.Is` treats them as
equal: callers only need "authentication failed", and the message is for people.

## Calling another service with a token

Mount `mw.WithClientAuth` on the client chain; see [client](client.md#the-client-chain).
