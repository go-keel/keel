# Redis

Read this before opening a Redis client, taking a distributed lock or rate-limiting a business action.

`core/kredis` opens go-redis clients. Two subpackages build on it: `core/kredis/lock` (distributed lock) and
`core/kredis/ratelimit` (sliding-window rate limiter for business code).

What kredis does for you:

- A `redis.UniversalClient`, so standalone, Sentinel and Cluster all work; one client per named config section.
- OpenTelemetry tracing from the global tracer provider. Observability is a process facility set up by `kotel.Setup`,
  not a constructor parameter; when it is off the global is a harmless no-op.
- A `Ping` with a 5-second timeout at construction, so an unreachable Redis fails startup instead of the first request.

## Configuration

```yaml
redis:
  default:
    addr: ["localhost:6379"]
    username: ""
    password: ""
    db: 0
    read_timeout: "3s"    # default 3s (go-redis); 0 means the default
    write_timeout: "3s"   # default 3s; 0 means the default
    pool_size: 0          # connections per node; 0 means the go-redis default (10 × GOMAXPROCS)

  # Sentinel: setting master_name selects Sentinel mode, and addr lists the sentinels
  session:
    addr: ["sentinel1:26379", "sentinel2:26379", "sentinel3:26379"]
    master_name: "mymaster"
    password: ""          # password of the data nodes
    sentinel_password: "" # password of the sentinels themselves, Sentinel mode only

  # Cluster: several addresses without master_name
  cluster:
    addr: ["node1:6379", "node2:6379", "node3:6379"]
```

## Assembly

`kredis.NewRedis(cfg)` opens `redis.default`, `kredis.NewNamedRedis(cfg, name)` any other section. Each returns a
cleanup that closes the client; run cleanups in reverse order (rules in [Assembly](assembly.md)). With several
clients, close the ones already opened when a later one fails:

```go
func NewData(cfg *kconfig.Config) (*Data, func(), error) {
    store, storeCleanup, err := kredis.NewNamedRedis(cfg, "default")
    if err != nil {
        return nil, nil, err
    }
    cache, cacheCleanup, err := kredis.NewNamedRedis(cfg, "cache")
    if err != nil {
        storeCleanup()
        return nil, nil, err
    }
    return &Data{store: store, cache: cache}, func() { cacheCleanup(); storeCleanup() }, nil
}
```

`kredis.Redis` embeds `redis.UniversalClient`, so every go-redis command (`Get`, `ZAdd`, `Pipeline`, ...) is called
on it directly; the embedding is also the raw handle. `redis.Nil` reports a missing key:

```go
data, err := c.redis.Get(ctx, fmt.Sprintf("note:%d", id)).Bytes()
if errors.Is(err, redis.Nil) {
    return nil, nil // cache miss
}
```

## Sharing Redis with kasynq

A Redis that also holds asynq's queues must not run an `allkeys-*` eviction policy (kasynq refuses to start, see
[Tasks](tasks.md)). Under `volatile-*`, give every cache key a TTL: keys without one are never evicted and fill
memory until Redis rejects writes, including enqueues.

## Distributed lock

`lock.NewLocker(rdb)` takes a lock with `SET key <token> NX EX <ttlSec>`: `NX` makes acquisition atomic and `EX` releases
the lock when the TTL ends without any cleanup process. The token is a random UUID only the holder knows, and release
is an atomic "delete only if the token matches", so when A's lock expires and B takes it, A's late release cannot
delete B's lock.

**Finish the locked work within `ttlSec`.** The lock is not renewed (no watchdog): when the TTL ends any instance can
take it, and work that runs over means two processes inside the critical section at once. Size `ttlSec` for the worst
case, generously. Renewal is left out on purpose: a watchdog must handle failed renewals, clock drift and GC pauses,
and its failure mode ("I think I hold it, but I lost it") is harder to diagnose than a TTL sized to the constraint.
For long transactions use a database row lock or a business idempotency key instead.

```go
unlock, err := uc.locker.TryAcquireLock(ctx, "note:publish:"+noteID, 30)
if err != nil {
    return err // lock.ErrLockNotAcquired: someone else is handling this note
}
defer unlock()
// critical section
```

- Always `defer` the returned release function. It uses its own short background timeout, so the lock is returned
  even when the caller's `ctx` is already cancelled, instead of waiting for the TTL.
- `TryAcquireLockWithTimeout` retries every 100 ms until the timeout and stops as soon as `ctx` is cancelled.
- A release that finds the lock no longer ours means the TTL constraint was broken and the critical section did
  overlap. The release function returns nothing (it runs in `defer`), so it logs this at ERROR with
  `ttl_exceeded=true`. Alert on that log line in production.

## Rate limiting

`ratelimit` limits business actions per user, per phone number or per operation. It is not a Kratos middleware: call
`Allow` from business code. It returns `nil` within the limit, `ratelimit.ErrLimitExceeded` over it, and a wrapped
error when Redis fails.

```go
limiter, err := ratelimit.New(rdb, 5*time.Second) // 5-second buckets
...
err := uc.limiter.Allow(ctx, "note:share:"+userID, 10, time.Minute) // 10 shares per minute
if errors.Is(err, ratelimit.ErrLimitExceeded) {
    return biz.ErrTooManyShares
}
if err != nil {
    return err // Redis failure
}
```

- One Lua script trims buckets outside the window, sums and increments the current bucket atomically, so there is no
  race between instances.
- **Storage depends on buckets, not on the limit**: each key is a hash with `window/bucket` fields. A one-hour window
  with 5-minute buckets is 12 fields whether the limit is 5 or 100,000.
- The price is quantisation: the counted span is between `window - bucket` and `window`, so the limiter errs on the
  lenient side and may let through about `limit × bucket / window` extra requests, but never rejects wrongly. Do not
  use it where the count must be exact.
- The bucket length is fixed in `New`; a window that is not a whole number of buckets, or a limit below 1, is an error.
