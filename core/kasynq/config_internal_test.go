// In-package: checks how the config section maps to asynq's config, which the constructors do not return.

package kasynq

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
)

// Every cronspec is interpreted in UTC, neither drifting with the machine's local time zone nor relying on asynq's
// default.
func TestSchedulerUsesUTC(t *testing.T) {
	if loc := schedulerOpts().Location; loc != time.UTC {
		t.Errorf("cron time zone = %v, must be UTC", loc)
	}
}

// conf.proto and docs/tasks.md both declare support for concurrency / queues / strict_priority; if they were not
// mapped, the configured values would have no effect.
func TestBuildAsynqConfig_MapsAllFields(t *testing.T) {
	cfg := buildAsynqConfig(&conf.Asynq{
		Concurrency:    8,
		Queues:         map[string]int32{"critical": 6},
		StrictPriority: true,
	}, 30*time.Second)
	if cfg.Concurrency != 8 {
		t.Errorf("Concurrency = %d, want 8", cfg.Concurrency)
	}
	if cfg.Queues["critical"] != 6 {
		t.Errorf("Queues = %v", cfg.Queues)
	}
	if !cfg.StrictPriority {
		t.Error("StrictPriority not mapped")
	}
}

// A missing config section is a hard error, with no fallback to asynq defaults.
func TestGetAsynqConfig_MissingSection(t *testing.T) {
	c := &kconfig.Config{Config: &conf.Config{Asynq: map[string]*conf.Asynq{"default": {}}}}
	if _, err := getAsynqConfig(c, "default"); err != nil {
		t.Fatalf("existing section: %v", err)
	}
	if _, err := getAsynqConfig(c, "other"); err == nil {
		t.Error("missing section should be an error, not asynq defaults")
	}
}

// In asynq v0.26.0, Scheduler.Shutdown unconditionally Closes the client built on the shared connection, so every
// graceful shutdown produces a false ERROR; schedulerLogger must downgrade that one to DEBUG without swallowing other,
// real errors.
func TestSchedulerLogger_DowngradesSharedCloseError(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	l := schedulerLogger{}
	l.Error("Failed to close redis client connection: redis connection is shared")
	if out := buf.String(); strings.Contains(out, "level=ERROR") || !strings.Contains(out, "level=DEBUG") {
		t.Errorf("the false shared-close error should be downgraded to DEBUG: %s", out)
	}

	buf.Reset()
	l.Error("scheduler heartbeat failed")
	if out := buf.String(); !strings.Contains(out, "level=ERROR") {
		t.Errorf("a real error must not be downgraded: %s", out)
	}
}

// asynq itself reads a zero asynq.Config.ShutdownTimeout as a hard-coded 8-second default (asynq v0.26.0
// server.go:416 defaultShutdownTimeout), not as "no limit". Without wiring the field to app.stop_timeout, "30 seconds
// configured, yet a 12-second in-flight task is forcibly cancelled and re-enqueued at 8 seconds" happens, and Stop
// even reports a "clean shutdown" because it finished within the outer ctx budget. buildAsynqConfig must set it to a
// positive value derived from app.stop_timeout, never leave it at 0.
func TestBuildAsynqConfig_ShutdownTimeoutDerivedFromAppStopTimeout(t *testing.T) {
	cfg := buildAsynqConfig(&conf.Asynq{}, 30*time.Second)
	if cfg.ShutdownTimeout <= 0 {
		t.Fatalf("ShutdownTimeout = %v; asynq reads <=0 as a hard-coded 8-second default, "+
			"so a configured app.stop_timeout would have no effect", cfg.ShutdownTimeout)
	}
	if cfg.ShutdownTimeout >= 30*time.Second {
		t.Errorf("ShutdownTimeout = %v; it should be slightly shorter than app.stop_timeout (30s), "+
			"leaving a margin for asynq's own wrap-up to finish first so the outer ctx does not race it",
			cfg.ShutdownTimeout)
	}
	want := 30*time.Second - asynqShutdownMargin
	if cfg.ShutdownTimeout != want {
		t.Errorf("ShutdownTimeout = %v, want %v", cfg.ShutdownTimeout, want)
	}
}

// When app.stop_timeout is shorter than asynqShutdownMargin, appStopTimeout - asynqShutdownMargin is non-positive;
// this branch must fall back to the original budget instead of letting the result slide back to <=0. asynq reads
// <=0 as a hard-coded 8-second default (see the previous test's comment), and "the budget is configured short"
// should not be worse than "not configured at all".
func TestBuildAsynqConfig_ShutdownTimeoutShorterThanMarginFallsBackToOriginal(t *testing.T) {
	const short = time.Second // shorter than asynqShutdownMargin (2s)
	cfg := buildAsynqConfig(&conf.Asynq{}, short)
	if cfg.ShutdownTimeout != short {
		t.Errorf("ShutdownTimeout = %v, want %v (should fall back to the original budget, "+
			"not compute <=0)", cfg.ShutdownTimeout, short)
	}
}
