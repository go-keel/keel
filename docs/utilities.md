# Utilities

Read this before using a package under `pkg/` (IDs, decimals, JWT, passwords, JSON) or adding a helper that
might already exist there. Signatures are in `go doc`; this page holds the constraints and their reasons.

Everything in `pkg/` except the snowflake generator is a stateless function or a value safe for concurrent use. The
distributed lock and the rate limiter are sub-packages of `kredis` and are covered in [redis.md](redis.md). Field
redaction (`pkg/redact`) is driven by a proto annotation and is covered in [proto.md](proto.md#redact).

## Snowflake IDs (`core/ksnowflake`)

IDs are 64-bit snowflakes with the `bwmarrin/snowflake` defaults: 41 bits of milliseconds, 10 bits of node ID
(0-1023), 12 bits of sequence (4096 IDs per millisecond per node). They grow with time, which keeps database indexes
and keyset pagination cheap.

- **One `*ksnowflake.Generator` per process, shared by every use case.** A node ID is unique only if one snowflake
  node uses it; a second generator with the same node ID would issue the same IDs in the same millisecond.
- **`app.node_id` picks the constructor.** A value 0-1023 is fixed: `ksnowflake.NewStatic(cfg)`, no Redis. `-1` leases
  a free node ID from Redis: `ksnowflake.NewAuto(cfg, rdb)`, whose cleanup releases it. The wrong one for the configured
  value fails at startup (`ksnowflake.allocator-mismatch`).
- **An unset `node_id` is 0 and is accepted as a fixed value.** Several instances that all omit it issue duplicate IDs
  without any error, so give each fixed instance its own value, or use `-1`.
- **`Next()` can fail with a leased node ID.** The lease (30s, renewed every 10s) must be confirmed within the last 20s;
  otherwise `Next` returns `ksnowflake.ErrLeaseExpired`, because another instance may soon own the node ID and a
  duplicate ID in a database cannot be taken back. Issuing resumes once renewal succeeds. If the node ID turns out to
  belong to another instance, the generator claims a free one and continues; the process does not exit. With a fixed
  node ID, `Next` never fails.
- **Keep the library's default epoch (2010-11-04) in every service.** IDs from services with different epochs can
  collide.

```go
// app.node_id = -1; with a fixed node_id use ksnowflake.NewStatic(cfg) and no Redis.
ids, cleanup, err := ksnowflake.NewAuto(cfg, rdb)
if err != nil {
	return err
}
defer cleanup()

// The use case declares the one method it needs, so tests can pass a fake.
type idSource interface{ Next() (ksnowflake.ID, error) }

func NewNoteUseCase(ids idSource, repo NoteRepo) *NoteUseCase { return &NoteUseCase{ids: ids, repo: repo} }
```

`ID.Base58()` gives a short form without the look-alike characters `0`, `O`, `I`, `l`, suited to codes people read
aloud or type.

## Decimal strings (`pkg/types`)

Converts between a [decimal string](proto.md#decimal-strings) field and `decimal.Decimal`.

- **Use `ParseDecimal` by default.** Use `DecimalOrZero` only where "not given means 0" is the business meaning;
  anywhere else a missing value silently becomes 0.
- **`DecimalString` writes the value as it is.** It does not truncate to the rule's 8 decimal places.
- **These functions handle strings and numbers, not presence.** An `optional` field is a `*string` in Go: read it with
  the generated `GetXxx()` (an absent field gives `""`), and take the address yourself when writing.
- **`ParseDecimal` does not repeat the rule's pattern.** The validation middleware enforces the shape; a string that
  did not pass validation (a raw route, an internal call) is parsed more leniently, accepting a plus sign and
  scientific notation.

```go
amount, err := types.ParseDecimal(req.GetAmount())
reply.Amount = types.DecimalString(note.Amount)
```

## JWT (`pkg/jwt`)

Signs and verifies JWTs with RS256 (asymmetric: the public key can be handed to every verifying service) or HS256
(symmetric: one shared secret). The server's authentication uses it; which algorithm to pick is in
[auth.md](auth.md).

- **Build claims with `NewClaims`.** `jwtv5.MapClaims` requires `exp`, `iat` and `nbf` as `float64` Unix seconds; an
  `int64` fails at sign or verify time with "invalid type for claim". Add custom claims to the returned map.
- **Every token has an expiry.** `Sign` refuses claims without `exp` (`ErrClaimsMissingExpirationTime`), and verifiers
  reject tokens without `exp` or past it.
- **A verifier accepts only its own algorithm**, so an HS256 token forged with the RSA public key as the HMAC secret
  is rejected by the RS256 verifier (algorithm confusion).
- **An empty HS256 secret is an error** (`ErrEmptySecret`) when the signer or verifier is built, not a token that
  anyone can forge.
- **Keys are PEM files**, parsed by golang-jwt: private keys PKCS#1 or PKCS#8, public keys PKIX (what `openssl rsa -pubout`
  writes), PKCS#1 or an X.509 certificate. An unreadable or invalid file fails with a rule error naming it
  (`jwt.key-file-unreadable`, `jwt.private-key-invalid`, `jwt.public-key-invalid`). Keep the private key only
  in the service that issues tokens. Generate a pair with
  `openssl genrsa -out private.pem 2048 && openssl rsa -in private.pem -pubout -out public.pem`.

```go
key, err := jwt.LoadRSAPrivateKey("configs/private.pem")
if err != nil {
	return err
}
claims := jwt.NewClaims(userID, 24*time.Hour)
claims["role"] = "editor"
token, err := jwt.NewRS256Signer(key).Sign(claims)
```

## Passwords (`pkg/pwd`)

Hashes and verifies passwords with Argon2id at `argon2id.DefaultParams` (64 MB memory, 1 iteration, parallelism = number of CPUs).
The hash string carries its parameters, so stored hashes stay verifiable if the defaults change.

- **`VerifyPassword` returns `false, nil` for a wrong password** and an error only for a malformed hash, so tell the
  two apart before answering the caller.
- **Limit the length in the contract**, with `buf.validate` on the request field (for example `min_len: 8,
  max_len: 72`). Argon2 accepts any length and `pkg/pwd` does not check it.
- **Hashing costs about 100 ms of CPU and 64 MB of memory per call by design.** Give sign-up and login endpoints a
  timeout that allows for it, and expect concurrent logins to need that memory at once.

## JSON (`pkg/json`)

A thin wrapper over `encoding/json/v2` that adds one option: map keys are written in sorted order
(`json.Deterministic(true)`). v2 writes map keys in iteration order, so the same value serialises differently each
time, which breaks log comparison, cache keys computed from output, and tests against literals.

- **Everything else is v2's default behaviour.** No v1 semantics are restored, so the Go documentation for
  `encoding/json/v2` describes it exactly.
- **Empty input unmarshals to nil error and leaves the target unchanged.** v2 reports `unexpected EOF`; an empty
  message body or cache value is normal, and this keeps "no content" apart from "broken content". Check the length
  first when empty must be an error.
- **`MarshalOrDefault` is for logs** and other places where a failed encoding is not worth handling.

## Rule errors (`pkg/rule`)

Builds developer-facing errors with rule IDs; see [errors.md](errors.md).

What is not here on purpose: digests, masking, pointer helpers and date ranges. They are a line of the standard
library (`crypto/sha256`, `new(v)`) or business decisions (what to mask, when a value counts as absent), so a service
writes them where it needs them.
