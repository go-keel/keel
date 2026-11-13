# Layered architecture

Read this before deciding where a piece of code goes in a service built on Keel, or how errors and logs travel between
layers.

Keel does not enforce layers: it is a framework and provides mechanisms. But several of those mechanisms (the
middleware chain, the sentinel error mapping, assembly) are designed around one layering, and code that ignores it
fights them. This page says why the layers are split this way and which Keel mechanism sits in which layer.

## The layers a request passes through

```
transport   HTTP / gRPC server, message consumers, task handlers
            Keel provides: server/api, core/knats, core/kasynq
                │
            middleware chain (mw.Default + combinators)
            recovery → tracing → metrics → logging → metadata → [auth] → validate → [sentinel]
                │
service     implements the interfaces defined in proto; converts DTOs to and from domain objects
                │
biz         business rules and transaction orchestration; defines the repo interfaces, knows no storage
                │
data        implements the repo interfaces; translates storage and remote errors into business errors
                │
infra       DB, Redis, NATS, other services
            Keel provides: core/kdb, core/kredis, core/knats, core/khttpclient
```

## Why each layer is split off

- **The service layer holds no business logic**, because its inputs and outputs are proto-generated types: logic
  written there follows the interface contract, and renaming a field ends up changing a business rule. It converts
  between proto messages and domain objects and calls biz, nothing more.
- **biz defines the repo interfaces instead of importing data**, so the dependency points the other way (data depends
  on biz): storage can be swapped and faked in tests. biz imports no storage package and not `api/`; every type it
  knows is its own.
- **data is where errors are translated.** `gorm.ErrRecordNotFound`, the Redis nil reply and gRPC statuses stop here;
  only business errors go up. The reason is the same as for the interfaces: if upper layers knew lower-layer error
  types, swapping storage would change biz.
- **Domain objects and storage models are two types**, even when their fields are identical. They change for
  different reasons (one follows the business, the other the table); merged, adding a `gorm` tag means the table
  shape dictates the business concept.

## Dependency direction

```
service ──► biz ◄── data
   │         │        │
  api/   domain objects  storage models
```

- Upper layers depend on lower ones, never the reverse; layers do not depend on siblings.
- data depends on biz (it implements biz's interfaces), not the other way round.
- Domain objects and storage models do not depend on each other.

## Where Keel's mechanisms sit

- **The middleware chain** is in the transport layer; it is changed through the `mw.With*` presets and
  `InsertAfter` / `InsertBefore` on the `mw.Slot*` anchors. What is in the chain by default and how to insert more:
  [middleware](middleware.md).
- **The sentinel** sits at the end of the chain, closest to the handler, and maps the business errors biz defines to
  external error codes. So the service layer returns errors **unchanged**: an `errors.New`, or a wrap without `%w`,
  in service hides the error from the sentinel. See [middleware](middleware.md).
- **Assembly** happens in the user's `di.go`, and construction order is dependency order: data, then biz, then
  service. The rules and the three constructor shapes are in [assembly](assembly.md).
- **Logging** is a process facility outside the dependency graph: every layer calls package-level
  `slog.XxxContext(ctx, ...)`, which links the record to the trace of the request.

## Errors and logs: log each error once

Log at the **conversion point**, where an error first turns into a different error:

| Layer | Error source | Handling | Log it |
|---|---|---|---|
| data | DB / Redis / third-party API | translate into a business error | yes, the original error, while translating |
| data | call to another service | pass through unchanged | no, the source service logged it |
| data | expected case (record not found) | return a business error | no |
| biz | business error from data | pass through or wrap | no |
| service | business error from biz | return unchanged | no |
| sentinel | error it does not recognise | fall back to Internal | yes |

```go
if err := r.db.WithContext(ctx).First(&po, id).Error; err != nil {
    if errors.Is(err, gorm.ErrRecordNotFound) {
        return nil, biz.ErrNoteNotFound      // expected case: no log
    }
    slog.ErrorContext(ctx, "find note", "error", err, "note_id", id)
    return nil, biz.ErrUnexpectedError       // log once, then translate
}
```

Logging again in an upper layer only yields two records and two stack traces of the same error, and whoever
investigates cannot tell one failure from two.

## Common questions

- **Can service call data directly?** No. Bypassing biz leaves no place for transaction boundaries and business rules,
  and once one such call exists, data's interfaces start growing methods shaped for service.
- **Can biz access the database directly?** No. biz knows only the repo interfaces it defines; that is the whole price
  of "swap storage without touching business code".
- **Which layer defines a repo interface?** biz. An interface belongs to its user, not its implementer: defined in
  data, it would make biz depend on data.
- **Where does logic that spans domains go?** In the biz of the domain that needs it, calling the other domain through
  an interface. Do not create a "common biz": such a package becomes the home of every piece of code nobody can place.
