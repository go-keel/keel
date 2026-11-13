package kasynq_test

import (
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kasynq"
	"github.com/go-keel/keel/core/kconfig"
)

// A config section under a misspelled name is silently dropped by DiscardUnknown, and a worker falling back to asynq
// defaults listens only on the default queue: critical tasks pile up silently while the service stays healthy. So
// the config section must exist.
func TestNewWorkerServer_MissingConfigSectionErrors(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_, err := kasynq.NewNamedWorkerServer(&kconfig.Config{Config: &conf.Config{}}, rdb, "default", nil)
	if err == nil {
		t.Fatal("missing asynq config section should error, not fall back to defaults")
	}
	if !strings.Contains(err.Error(), "asynq.default") {
		t.Errorf("error should name the section: %v", err)
	}
}
