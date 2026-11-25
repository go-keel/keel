package kasynq

import (
	"errors"
	"log/slog"

	"github.com/go-keel/keel/internal/redislease"
)

// lease is the Redis key that makes one SchedulerServer replica of an application the only one enqueuing cron
// tasks: asynq's scheduler has no leader election, so N replicas would enqueue every trigger N times. Timings and the
// owner-checked scripts are shared with ksnowflake's node ID lease (internal/redislease).
type lease struct {
	key   string
	owner string // appid.LeaseOwner: the instance ID (so logs match the instance) plus a per-process token
}

// releaseLease deletes the lease if this instance holds it, so a standby takes over within 10s instead of 30s.
func (s *SchedulerServer) releaseLease() {
	if err := redislease.Release(s.rdb, s.lease.key, s.lease.owner); err != nil && !errors.Is(err, redislease.ErrNotHeld) {
		slog.Error("cron scheduler lease release failed; it expires within 30s", "key", s.lease.key, "error", err)
	}
}
