// Package redislease holds the Redis lease shared by ksnowflake's node ID and kasynq's scheduler leader: one key, set
// with SET NX to an owner token, renewed and released only by that owner. Each caller keeps its own key names, logs
// and what it does once the lease is lost; kredis/lock uses Release for the same owner-checked delete.
package redislease

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// The timings are fixed, not config: the holder must stop acting before the lease can expire (StopAfter < TTL), and a
// configurable pair could be set the other way round.
const (
	TTL            = 30 * time.Second
	Every          = 10 * time.Second // renewal by the holder, acquisition attempts by standbys
	StopAfter      = 20 * time.Second // two missed renewals; 10s margin for network delay and clock drift
	releaseTimeout = 3 * time.Second
)

// ErrNotHeld means the key is absent or holds another owner's token.
var ErrNotHeld = errors.New("lease held by another owner or expired")

// Acquire sets key to owner for TTL if the key is absent; ErrNotHeld means another owner holds it.
func Acquire(ctx context.Context, rdb redis.UniversalClient, key, owner string) error {
	ok, err := rdb.SetNX(ctx, key, owner, TTL).Result()
	if err == nil && !ok {
		return ErrNotHeld
	}
	return err
}

var renewScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("expire", KEYS[1], ARGV[2])
end
return 0
`)

// Renew extends key to TTL if owner still holds it; ErrNotHeld means it was lost.
func Renew(ctx context.Context, rdb redis.UniversalClient, key, owner string) error {
	n, err := renewScript.Run(ctx, rdb, []string{key}, owner, int(TTL.Seconds())).Int64()
	if err == nil && n != 1 {
		return ErrNotHeld
	}
	return err
}

var releaseScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0
`)

// Release deletes key if owner still holds it; ErrNotHeld means it had expired or passed to another owner. It runs on
// its own short timeout rather than the caller's ctx, which is often already canceled at shutdown.
func Release(rdb redis.UniversalClient, key, owner string) error {
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	n, err := releaseScript.Run(ctx, rdb, []string{key}, owner).Int64()
	if err == nil && n != 1 {
		return ErrNotHeld
	}
	return err
}
