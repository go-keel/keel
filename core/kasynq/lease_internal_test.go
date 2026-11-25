// In-package: checks the scheduler lease directly; its helpers are shared with scheduler_internal_test.go.

package kasynq

import (
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
)

func schedulerTestConfig() *kconfig.Config {
	return &kconfig.Config{Config: &conf.Config{App: &conf.App{Name: "svc", Id: "a"}}}
}

// The lease key carries the application's domain and name, so applications sharing one Redis do not block each other.
func TestNewSchedulerServer_LeaseKey(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	cfg := schedulerTestConfig()
	cfg.App.Domain = "shop"
	s, err := NewSchedulerServer(cfg, rdb, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.lease.key != "shop:svc:cron_scheduler" || !strings.HasPrefix(s.lease.owner, "a/") {
		t.Fatalf("lease = %q owned by %q, want shop:svc:cron_scheduler owned by instance a plus a token", s.lease.key, s.lease.owner)
	}
}
