package keel

import (
	"context"
	"io"
	"log/slog"
	"os"

	kratoslog "github.com/go-kratos/kratos/v3/log"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/trace"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
)

// SetupLogger assembles the process default logger (slog.SetDefault) with two outputs: console (kratos text
// handler + trace extractor) and OpenTelemetry (otelslog bridge). When neither is enabled it falls back to a
// stdout console so logs are not silently dropped.
//
// It deliberately returns nothing: logging is a process facility, not a node in the dependency graph. A function
// with no return type is naturally not a provider in wire style, so the language itself states this. Business and
// framework code call the standard library's package-level functions directly:
//
//	slog.InfoContext(ctx, "order paid", "order_id", id)  // inside a request: trace correlated automatically
//	slog.Info("server starting", "addr", addr)           // no request context
//
// It takes no observability parameter: whether the OTEL output is on, and which LoggerProvider it goes to, depends
// only on `otel.enabled`. otelslog.NewHandler without an explicit provider uses the global
// otel.GetLoggerProvider() (kotel.Setup registers exactly that global, and it delegates lazily, so building the
// logger before kotel.Setup does not leave it stuck on the no-op implementation).
func SetupLogger(config *kconfig.Config) {
	c := config.Logger
	if c == nil {
		c = &conf.Logger{Level: "info", StacktraceLevel: "error", Console: true}
	}

	level := parseLevel(c.Level, slog.LevelInfo)
	stackLevel := parseLevel(c.StacktraceLevel, slog.LevelError)

	// The allowed values (""/text/json) are enforced by the rules on conf.proto when config loads; here we only distinguish json.
	format := kratoslog.FormatText
	if c.Format == "json" {
		format = kratoslog.FormatJSON
	}

	var handlers []slog.Handler

	// Console output: kratos log.NewHandler = text/json format + source + trace extractor
	if c.Console {
		handlers = append(handlers, consoleHandler(os.Stdout, level, format))
	}

	// OpenTelemetry output: otelslog bridge → global LoggerProvider.
	if config.Otel.GetEnabled() {
		handlers = append(handlers, otelslog.NewHandler("keel"))
	}

	// Fallback: with neither console nor otel output, fall back to a stdout console so logs are not silently
	// dropped (especially dangerous when the default config leaves Console at its zero value, false).
	if len(handlers) == 0 {
		slog.Warn("no log outputs configured (console off, otel absent); falling back to stdout console")
		handlers = append(handlers, consoleHandler(os.Stdout, level, format))
	}

	var h slog.Handler
	if len(handlers) == 1 {
		h = handlers[0]
	} else {
		h = slog.NewMultiHandler(handlers...)
	}

	// The stack wrapper sits outside the fan-out, so it applies to both console and otel outputs.
	h = newStacktraceHandler(h, stackLevel)

	slog.SetDefault(slog.New(h))
}

// consoleHandler builds the console slog handler. Trace correlation happens in the handler layer via the
// extractor: callers only need the ctx variants (InfoContext and so on), with no manual injection.
func consoleHandler(w io.Writer, level slog.Level, format kratoslog.Format) slog.Handler {
	return kratoslog.NewHandler(
		kratoslog.WithWriter(w),
		kratoslog.WithFormat(format),
		kratoslog.WithLevel(level),
		kratoslog.WithAddSource(true),
		kratoslog.WithExtractor(traceExtractor),
	)
}

// traceExtractor extracts trace_id/span_id from the span in ctx and attaches them to the log record.
// The otel output does not need this: otelslog reads the SpanContext from ctx natively.
func traceExtractor(ctx context.Context) []slog.Attr {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []slog.Attr{
		slog.String("trace_id", sc.TraceID().String()),
		slog.String("span_id", sc.SpanID().String()),
	}
}

// parseLevel parses a level setting whose values conf.proto has already restricted to ""/debug/info/warn/error;
// empty means def. The log level defaults to info (so logs are not silently dropped); the stack threshold defaults to
// error (only error and above get a stack, so normal logs are not buried in stack traces).
func parseLevel(s string, def slog.Level) slog.Level {
	if s == "" {
		return def
	}
	var lv slog.Level
	_ = lv.UnmarshalText([]byte(s)) // cannot fail: the values are validated when config loads
	return lv
}
