package kdb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/go-keel/keel/conf"
)

// defaultSlowThreshold matches gorm's logger.Default.
const defaultSlowThreshold = 200 * time.Millisecond

// slogLogger routes gorm's logs into Keel's slog pipeline, so SQL slow queries/errors share a source with the rest
// of the logs (same trace correlation, same output) instead of going through gorm's default stdout writer.
//
// Why implement logger.Interface rather than just a logger.Writer adapter: gorm offers only these two extension
// points, and Writer's signature is Printf(string, ...any): gorm decides the level, formats the result into a
// string and passes it in, with neither the level nor ctx. Moreover, with Colorful=false traceWarnStr and
// traceErrStr are the same format string, so an adapter could not even tell "slow query" from "failure". Keeping
// the level, trace correlation and structured fields requires implementing the whole Interface.
//
// The fields map one-to-one to gorm's logger.Config, and the dispatch structure copies gorm's original (fc() is
// called only in branches that actually log: internally it runs db.Explain to build the full SQL, so hoisting it
// out of the switch would build it for every query for nothing).
type slogLogger struct {
	level         gormlogger.LogLevel
	slowThreshold time.Duration
}

func newSlogLogger(c *conf.Database) gormlogger.Interface {
	return &slogLogger{
		level:         parseGormLevel(c.GetLogLevel()),
		slowThreshold: durationOr(c.GetSlowThreshold(), defaultSlowThreshold),
	}
}

// durationOr returns d when it is positive and def otherwise: 0 and omission mean the default, and conf.proto rejects
// negative values when config loads.
func durationOr(d *durationpb.Duration, def time.Duration) time.Duration {
	if v := d.AsDuration(); v > 0 {
		return v
	}
	return def
}

// parseGormLevel parses database.<name>.log_level, whose values conf.proto has already restricted; empty means warn.
func parseGormLevel(s string) gormlogger.LogLevel {
	switch s {
	case "silent":
		return gormlogger.Silent
	case "error":
		return gormlogger.Error
	case "warn":
		return gormlogger.Warn
	case "info":
		return gormlogger.Info
	}
	return gormlogger.Warn
}

func (l *slogLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	nl := *l
	nl.level = level
	return &nl
}

// gorm calls Info/Warn/Error with printf conventions: msg is the format string and args are its arguments. Treating
// msg as a full sentence and putting args into separate fields would leave unexpanded %s / %v in the log.
func (l *slogLogger) Info(ctx context.Context, msg string, args ...any) {
	if l.level >= gormlogger.Info {
		slog.InfoContext(ctx, "gorm: "+fmt.Sprintf(msg, args...))
	}
}

func (l *slogLogger) Warn(ctx context.Context, msg string, args ...any) {
	if l.level >= gormlogger.Warn {
		slog.WarnContext(ctx, "gorm: "+fmt.Sprintf(msg, args...))
	}
}

func (l *slogLogger) Error(ctx context.Context, msg string, args ...any) {
	if l.level >= gormlogger.Error {
		slog.ErrorContext(ctx, "gorm: "+fmt.Sprintf(msg, args...))
	}
}

// Trace records each SQL statement's duration/rows affected/error: errors → Error, slow queries → Warn, and the
// rest only when level is info. The ctx variants keep trace correlation.
//
// ErrRecordNotFound is never logged as an error (gorm's IgnoreRecordNotFoundError defaults to false; this
// deliberately deviates): no matching record is a normal business branch, and logging it as an error is pure noise.
// Likewise context.Canceled is logged only at DEBUG: the caller disconnected midway, the request context was
// canceled and the in-flight query was interrupted with it, which is not a system failure; DeadlineExceeded means
// the server itself waited too long and is still logged as an error.
func (l *slogLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if l.level <= gormlogger.Silent {
		return
	}
	elapsed := time.Since(begin)

	switch {
	case errors.Is(err, context.Canceled):
		sql, rows := fc()
		slog.DebugContext(ctx, "gorm: query canceled",
			"error", err, "elapsed_ms", elapsed.Milliseconds(), "rows", rows, "sql", sql)
	case err != nil && l.level >= gormlogger.Error && !errors.Is(err, gorm.ErrRecordNotFound):
		sql, rows := fc()
		slog.ErrorContext(ctx, "gorm: query failed",
			"error", err, "elapsed_ms", elapsed.Milliseconds(), "rows", rows, "sql", sql)
	case l.slowThreshold > 0 && elapsed > l.slowThreshold && l.level >= gormlogger.Warn:
		sql, rows := fc()
		slog.WarnContext(ctx, "gorm: slow query",
			"elapsed_ms", elapsed.Milliseconds(), "threshold_ms", l.slowThreshold.Milliseconds(), "rows", rows, "sql", sql)
	case l.level == gormlogger.Info:
		sql, rows := fc()
		slog.InfoContext(ctx, "gorm: query",
			"elapsed_ms", elapsed.Milliseconds(), "rows", rows, "sql", sql)
	}
}
