package kasynq

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hibiken/asynq"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
	"github.com/go-keel/keel/internal/errs"
)

func getAsynqConfig(c *kconfig.Config, name string) (*conf.Asynq, error) {
	// Indexing a nil map safely returns the zero value, so no extra nil check is needed.
	if tc := c.Asynq[name]; tc != nil {
		return tc, nil
	}
	// A missing config section is a hard error, with no fallback to asynq defaults. Config parsing uses protojson's
	// DiscardUnknown, so a whole section under a misspelled name (say, task:) raises no error and is just dropped.
	// Falling back to defaults would make the worker listen only on the default queue: tasks enqueued to queues such
	// as critical would pile up silently while the service stays healthy. "Runs with asynq defaults" is not "runs
	// correctly".
	return nil, errs.Rules.New("kasynq.config-missing", fmt.Sprintf("the config has no asynq.%s section", name),
		fmt.Sprintf("add `asynq: {%s: {}}` to the config", name))
}

// schedulerLogger is a thin layer over slogLogger used only by SchedulerServer: in asynq v0.26.0,
// Scheduler.Shutdown unconditionally Closes its internal client (scheduler.go:304), and because that client is built
// on a shared connection, Close always returns an error, so every graceful shutdown would log a false ERROR. This
// logger downgrades that one message to DEBUG. If upstream changes the wording, the downgrade stops matching and the
// noise comes back, but no real error is lost.
type schedulerLogger struct{ slogLogger }

func (l schedulerLogger) Error(args ...any) {
	if strings.HasPrefix(fmt.Sprint(args...), "Failed to close redis client connection") {
		l.Debug(args...)
		return
	}
	l.slogLogger.Error(args...)
}

// slogLogger routes asynq's own logs to slog.
//
// Without it, server start and stop, redis connection failures and worker errors would all go through asynq's
// built-in stderr logger instead of the unified output, the same reason kdb has an adapter for gorm.
type slogLogger struct{}

func (slogLogger) Debug(args ...any) { slog.Debug(fmt.Sprint(args...), "source", "asynq") }
func (slogLogger) Info(args ...any)  { slog.Info(fmt.Sprint(args...), "source", "asynq") }
func (slogLogger) Warn(args ...any)  { slog.Warn(fmt.Sprint(args...), "source", "asynq") }
func (slogLogger) Error(args ...any) { slog.Error(fmt.Sprint(args...), "source", "asynq") }

// Fatal only logs; it does not exit the process.
//
// The comment on asynq's Logger interface says "process will exit with status set to 1", but nothing in the v0.26.0
// implementation calls it (only the interface forwarding in internal/log), so for now this method exists purely to
// satisfy the interface. Even if it is called, it does not exit: whether the process lives or dies is for the user's
// health checks to decide, not the framework. A logging method that takes down the whole process causes the hardest
// kind of outage to diagnose.
func (slogLogger) Fatal(args ...any) {
	slog.Error(fmt.Sprint(args...), "source", "asynq", "fatal", true)
}

// asynqShutdownMargin is how much asynq's internal shutdown deadline is narrowed relative to appStopTimeout.
//
// asynq.Server.Shutdown takes no ctx and honours only asynq.Config.ShutdownTimeout, fixed at construction
// (kasynq.WorkerServer.Stop adds an outer ctx bound; see its comment). If the two budgets were exactly equal they
// would race within a millisecond window: after asynq forcibly puts in-flight tasks back on the queue at its
// deadline, its own wrap-up, such as closing the broker connection, still takes a moment, and the outer ctx could
// expire first and misreport "asynq already finished cleanly" as "gave up after waiting for the timeout". The margin
// lets asynq's internal deadline arrive first and finish wrapping up reliably; the outer ctx steps in only as the
// last line of defence when asynq cannot cope either (for example, broker.Close hangs).
const asynqShutdownMargin = 2 * time.Second

func buildAsynqConfig(tc *conf.Asynq, appStopTimeout time.Duration) asynq.Config {
	cfg := asynq.Config{StrictPriority: tc.StrictPriority, Logger: slogLogger{}}
	// asynq itself reads a zero asynq.Config.ShutdownTimeout as a hard-coded 8-second default (asynq v0.26.0
	// server.go:416 defaultShutdownTimeout), not as "no limit". Without wiring app.stop_timeout in here, it degrades
	// to "configured 30 seconds, cut off at 8", and the promise that there is only one number to configure would not
	// hold for tasks. appStopTimeout is already the result of kconfig.Config.StopTimeout() with the default applied,
	// so only the margin is subtracted here; a non-positive result means the budget was shorter than the margin to
	// begin with, so the original value is used instead, never falling back to 0 and triggering asynq's default.
	cfg.ShutdownTimeout = appStopTimeout - asynqShutdownMargin
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = appStopTimeout
	}
	// Non-negative concurrency, non-blank queue names and positive weights are enforced by the rules in conf.proto
	// at config load (asynq silently falls back or silently filters for each of these); 0 keeps its meaning of "use
	// the asynq default".
	cfg.Concurrency = int(tc.Concurrency)
	if len(tc.Queues) > 0 {
		queues := make(map[string]int, len(tc.Queues))
		for k, v := range tc.Queues {
			queues[k] = int(v)
		}
		cfg.Queues = queues
	}
	return cfg
}
