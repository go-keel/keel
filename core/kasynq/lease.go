package kasynq

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// The lease timings are fixed, not config: scheduling must stop before the lease can expire (leaseStopAfter <
// leaseTTL), and a configurable pair could be set the other way round. Same timings as ksnowflake's node ID lease.
const (
	leaseTTL            = 30 * time.Second
	leaseEvery          = 10 * time.Second // renewal by the holder, acquisition attempts by standbys
	leaseStopAfter      = 20 * time.Second // two missed renewals; 10s margin for network delay and clock drift
	leaseReleaseTimeout = 3 * time.Second
)

// lease is the Redis key that makes one SchedulerServer replica of an application the only one enqueuing cron
// tasks: asynq's scheduler has no leader election, so N replicas would enqueue every trigger N times.
type lease struct {
	rdb   redis.UniversalClient
	key   string
	owner string // appid.LeaseOwner: the instance ID (so logs match the instance) plus a per-process token
}

func (l lease) acquire(ctx context.Context) (bool, error) {
	return l.rdb.SetNX(ctx, l.key, l.owner, leaseTTL).Result()
}

var leaseRenewScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("expire", KEYS[1], ARGV[2])
end
return 0
`)

// renew extends the lease; false means another instance holds it.
func (l lease) renew(ctx context.Context) (bool, error) {
	n, err := leaseRenewScript.Run(ctx, l.rdb, []string{l.key}, l.owner, int(leaseTTL.Seconds())).Int64()
	return n == 1, err
}

var leaseReleaseScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0
`)

// release deletes the lease if this instance holds it, so a standby takes over within leaseEvery instead of leaseTTL.
func (l lease) release() {
	ctx, cancel := context.WithTimeout(context.Background(), leaseReleaseTimeout)
	defer cancel()
	if _, err := leaseReleaseScript.Run(ctx, l.rdb, []string{l.key}, l.owner).Int64(); err != nil {
		slog.Error("cron scheduler lease release failed; it expires within 30s", "key", l.key, "error", err)
	}
}
