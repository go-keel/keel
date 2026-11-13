// Package ratelimit provides a Redis-backed sliding window rate limiter for
// business-layer use (per-user, per-phone, per-operation limiting). This is
// NOT a kratos middleware.Middleware — call Allow() directly in biz code.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/internal/errs"
)

// ErrLimitExceeded is returned when the rate limit for a key is exceeded.
var ErrLimitExceeded = errors.New("ratelimit: limit exceeded")

// bucketedWindowScript atomically trims buckets outside the window, sums the counts inside the window and, if the
// limit is not exceeded, adds 1 to the current bucket. It returns 1 to allow and 0 to reject.
//
// Time comes from redis TIME rather than the calling process: in a multi-instance deployment machine clocks drift,
// and with local clocks the counts written by a slow machine would be trimmed at once by other instances using
// their own window start, inflating that instance's effective limit, while rate limiting exists precisely to
// prevent abuse.
//
// KEYS[1] = key
// ARGV[1] = bucket (ms), ARGV[2] = bucket count, ARGV[3] = limit, ARGV[4] = ttl (s)
var bucketedWindowScript = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local bucketMs = tonumber(ARGV[1])
local cur = math.floor(now / bucketMs)
local oldest = cur - tonumber(ARGV[2]) + 1

local sum = 0
local stale = {}
local data = redis.call('HGETALL', KEYS[1])
for i = 1, #data, 2 do
  if tonumber(data[i]) < oldest then
    stale[#stale + 1] = data[i]
  else
    sum = sum + tonumber(data[i + 1])
  end
end
if #stale > 0 then
  redis.call('HDEL', KEYS[1], unpack(stale))
end

if sum + 1 > tonumber(ARGV[3]) then
  return 0
end
redis.call('HINCRBY', KEYS[1], cur, 1)
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[4]))
return 1
`)

// Limiter is a bucketed sliding window rate limiter backed by Redis.
//
// The window is split into counting buckets of size bucket; each key in redis is a HASH whose field is the bucket
// index and whose value is that bucket's count. Storage is window/bucket fields, **independent of limit**: changing
// limit from 5 to 100,000 does not change memory. (The earlier approach of writing one ZSET member per request was
// O(limit): with limit=100,000 a single key took about 10MB.)
//
// The cost is that the window is quantized to bucket boundaries: the window must be a whole number of buckets, and the
// span actually counted falls within [window-bucket, window], erring on the lenient side. In the extreme case about limit×bucket/window extra
// requests may be allowed within a window, but none are wrongly rejected. For uses like "N SMS per phone number
// per hour", a wrong rejection is a user-visible complaint while an extra allowance goes unnoticed; do not use this
// limiter when exact counting is required.
type Limiter struct {
	rdb    redis.UniversalClient
	bucket time.Duration
}

// New creates a Limiter whose windows are counted in buckets of the given
// length. Smaller buckets mean finer windows and more redis fields per key
// (window/bucket), e.g. a 1h window with 5min buckets keeps 12 fields.
func New(rdb redis.UniversalClient, bucket time.Duration) (*Limiter, error) {
	if bucket <= 0 {
		return nil, errs.Rules.New("kredis.ratelimit-bucket", fmt.Sprintf("ratelimit.New was called with bucket %v", bucket), "pass a positive bucket duration, such as time.Minute")
	}
	return &Limiter{rdb: rdb, bucket: bucket}, nil
}

// Allow checks whether key is within its limit for the given window.
// Returns ErrLimitExceeded if exceeded, nil if allowed, or a wrapped error on
// Redis failure, on a window that is not a whole number of buckets, or on a limit below 1.
func (l *Limiter) Allow(ctx context.Context, key string, limit int, window time.Duration) error {
	// A window that is not a whole number of buckets would be counted over a different span than asked for (rounded up it
	// rejects requests the window allows, rounded down it silently shortens the window), so it is refused.
	if window < l.bucket || window%l.bucket != 0 {
		return errs.Rules.New("kredis.ratelimit-window", fmt.Sprintf("Allow was called with window %v, not a whole number of the Limiter's %v buckets", window, l.bucket), "pass a window that is a multiple of the bucket, or create the Limiter with a bucket that divides the window")
	}
	// A limit below 1 would reject every request, which is a missing or zero config value far more often than intent.
	if limit < 1 {
		return errs.Rules.New("kredis.ratelimit-limit", fmt.Sprintf("Allow was called with limit %d", limit), "pass a limit of at least 1, and check the config value it is read from is set")
	}
	buckets := int(window / l.bucket)
	ttl := int((time.Duration(buckets) * l.bucket).Seconds()) + 1

	res, err := bucketedWindowScript.Run(ctx, l.rdb,
		[]string{key},
		l.bucket.Milliseconds(), buckets, limit, ttl,
	).Int()
	if err != nil {
		return fmt.Errorf("ratelimit: redis error: %w", err)
	}
	if res == 0 {
		return ErrLimitExceeded
	}
	return nil
}
