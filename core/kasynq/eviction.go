package kasynq

import (
	"context"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/internal/errs"
)

// checkEvictionPolicy refuses a Redis whose maxmemory-policy may evict any key (allkeys-*): under memory pressure it
// would silently delete asynq's queues, so tasks vanish without an error. noeviction and volatile-* are safe as long
// as the keys sharing that Redis (e.g. cache entries) carry a TTL, since volatile-* only evicts keys with one. Read via
// INFO memory, because managed Redis services often disable CONFIG GET. Runs when the worker or scheduler starts.
func checkEvictionPolicy(ctx context.Context, rdb redis.UniversalClient) error {
	info, err := rdb.Info(ctx, "memory").Result()
	if err != nil {
		return fmt.Errorf("kasynq: read maxmemory_policy with INFO memory: %w", err)
	}
	return evictionPolicyError(info)
}

func evictionPolicyError(info string) error {
	for line := range strings.SplitSeq(info, "\n") {
		policy, ok := strings.CutPrefix(strings.TrimSpace(line), "maxmemory_policy:")
		if !ok {
			continue
		}
		if strings.HasPrefix(policy, "allkeys-") {
			return errs.Rules.New("kasynq.eviction-policy",
				fmt.Sprintf("Redis maxmemory-policy is %s, which can evict asynq's queues and lose tasks silently", policy),
				"set maxmemory-policy to volatile-lru (cache keys must then have a TTL) or noeviction, or give asynq its own Redis")
		}
		return nil
	}
	return errs.Rules.New("kasynq.eviction-unknown",
		"Redis INFO memory has no maxmemory_policy field, so Keel cannot verify that Redis will not evict asynq's queues",
		"point asynq at a Redis server that reports maxmemory_policy in INFO memory")
}
