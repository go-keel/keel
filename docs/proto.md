# Proto

Read this before writing a `.proto` contract that uses Keel's annotations or rules: anonymous endpoints, redacted
fields, decimal strings, or your own extension numbers.

Keel's `.proto` files live in `proto/keel/v1/`: `annotations.proto` (the `auth` and `redact` options) and
`rules.proto` (the `decimal` rule). Import them in `.proto` files as `keel/v1/annotations.proto` and
`keel/v1/rules.proto`; the generated Go package is `github.com/go-keel/keel/proto/keel/v1` (imported as `keelpb`).

## Where the files come from

Keel's `.proto` files ship inside the Go module, not through the Buf Schema Registry. A service
copies `proto/keel/` and `third_party/` (protovalidate and googleapis) from the Keel version in its `go.mod`, commits
the copies and keeps them identical to that version whenever it upgrades Keel. The Go module is then the only version source: a `.proto` copy newer
than the linked Go package would let a new validation rule compile and then be skipped silently at runtime.

- **Generate only your own files.** Map Keel's import to the Go package above and never regenerate Keel's files with
  `buf generate --include-imports`: a second registration of the same extension number panics in `init`.
- **Take extension numbers from your own range.** Keel reserves 50001-51000 (`auth` 50001, `redact` 50002, `decimal`
  50003, assigned in order whatever the extendee, so the ledger never shows one number twice). Applications take
  ranges of 1000 from 51001 upwards and declare them in their own `.proto` files.

## HTTP routes

Routes come from the standard `google.api.http` option, read by Kratos' `protoc-gen-go-http`; Keel adds no routing
annotation. Keep an internal endpoint out of the public contract by putting it in its own `.proto` file and leaving
that file out of OpenAPI generation (see [http.md](http.md)).

## Anonymous endpoints

Every endpoint requires authentication unless its method carries `allow_anonymous`. The server's authentication
lets requests without a token through only on those methods (see [auth.md](auth.md)).

```protobuf
import "keel/v1/annotations.proto";

rpc ListPublicNotes(ListPublicNotesRequest) returns (ListPublicNotesResponse) {
  option (keel.v1.auth) = {allow_anonymous: true};
}
```

## Redact

`(keel.v1.redact)` marks a field whose value must not reach side records: audit logs, request logs. Two switches share
one scrubbing path.

| Switch | Replaced by | Use it for |
|---|---|---|
| `sensitive` | `"***"` | values that must not leak: passwords, one-time codes, tokens, an `otpauth://` URI |
| `bulky` | `"[omitted 1234 bytes]"` | values that are harmless but large: base64 images, file contents |

```protobuf
message CreateNoteRequest {
  string title = 1;
  string share_token = 2 [(keel.v1.redact) = {sensitive: true}];
  // A base64 image can push the whole log line past the size limit, and truncation also drops the fields after it.
  string cover_base64 = 3 [(keel.v1.redact) = {bulky: true}];
}
```

The logging middleware applies it to request and response bodies once body logging is on (`mw.WithLogging`, see
[middleware.md](middleware.md)); call `redact.Redact(msg)` for any side record you write yourself.

- **A misplaced annotation fails startup.** The annotation works only on `string` fields (including
  `repeated string` and string map values); `mw.Default` runs `redact.Validate`, which rejects it anywhere else, because
  redaction fails by leaking silently and the first production request is too late to find out.
- **`bulky` keeps the byte count, `sensitive` does not.** A password's length is information; an image's size is
  what you need when debugging an upload. With both set, `sensitive` wins.
- **`bulky` is not a security measure.** Mark secrets `sensitive`, so any later rule about who may see redacted
  values keys on the right switch.
- **Mark the field once, on the innermost `string`.** Sensitivity belongs to the field, so every endpoint using the
  message is covered, and nested messages, lists and maps are walked recursively.
- **An unmarked field is logged as it is.** Keel does not guess from field names: a pattern like `.*token.*` hits
  `token_type` and misses `provisioning_uri`. Marking a new secret field is part of review.
- **`google.protobuf.Any` is unpacked**, because protojson expands it by `type_url` when logging; a message containing
  `Any` therefore always takes the copying path. An `Any` whose type cannot be resolved passes through unchanged,
  since protojson cannot expand it either.
- **Payloads that are not proto messages are logged as they are**: there is no annotation to go by.

## Decimal strings

Amounts, rates and quantities with a fractional part are plain `string` fields with the `keel.v1.decimal` rule, never
`float` or `double`, which lose precision. In JSON the field is `"price": "123.45"`.

```protobuf
import "buf/validate/validate.proto";
import "keel/v1/rules.proto";

message CreateNoteRequest {
  string price = 1 [(buf.validate.field).string.(keel.v1.decimal) = true];             // required
  optional string discount = 2 [(buf.validate.field).string.(keel.v1.decimal) = true]; // may be omitted
}
```

protovalidate runs the rule in the validation middleware of `mw.Default`, so a malformed value is rejected with 400
before your code runs. Keel uses a string plus a shared rule rather than a wrapper message because a message adds a
level in JSON (`{"value": "123.45"}`) and proto has no type alias; the cost is that the generated Go type is only
`string`. Convert with `pkg/types` ([utilities.md](utilities.md#decimal-strings-pkgtypes)).

**Format.** An optional leading `-` (`"-0"` is valid; treat it as 0); an integer part of `0` or up to 20 digits not
starting with 0; an optional fraction of up to 8 digits. Rejected: `"01"`, `"+1"`, `"1e3"`, `"100."`, `".5"`. A MySQL
`DECIMAL(28, 8)` column holds every valid value.

**Required or not is decided by `optional`.** The rule itself never accepts an empty string, so a forgotten
`optional` shows up as a 400 on the first request, not as a missing amount silently becoming 0.

| Declaration | Absent or `null` | `""` | Valid value |
|---|---|---|---|
| `string` | rejected | rejected | accepted |
| `optional string` | accepted (not validated) | rejected | accepted |

A client omits an optional value by leaving the key out or sending `null`, never by sending an empty string from an
empty form input.

In Go an `optional` field is a pointer. Read it with the generated `GetXxx()` (absent gives the zero value) or check
`!= nil` when absent and zero mean different things; set it with `new(v)` (Go 1.26). Whether an empty or zero domain
value means "absent" is a decision per field, so Keel offers no helper that makes it for you.

**The rule only guarantees a valid decimal number.**

- **Tighter digit limits** that match your storage columns are your own rules, written like `rules.proto`. A file
  that defines rules must be proto2 or editions, because proto3 cannot extend protovalidate's rule types; files that
  use the rules stay proto3. Take the extension number from your own range.
- **Ranges such as "must be positive" or "rate below 1"** are business meaning: check them in the business layer.
