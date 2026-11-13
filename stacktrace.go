package keel

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
)

// stacktraceHandler attaches a stack attribute to a record when its level reaches the threshold.
// slog itself does not record stacks by level; this wrapper provides the conf.Logger.StacktraceLevel semantics
// and applies to both the console and otel outputs (it wraps outside the fan-out).
type stacktraceHandler struct {
	next  slog.Handler
	level slog.Level
}

func newStacktraceHandler(next slog.Handler, level slog.Level) *stacktraceHandler {
	return &stacktraceHandler{next: next, level: level}
}

func (h *stacktraceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *stacktraceHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= h.level {
		r = r.Clone()
		r.AddAttrs(slog.String("stack", captureStack()))
	}
	return h.next.Handle(ctx, r)
}

func (h *stacktraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &stacktraceHandler{next: h.next.WithAttrs(attrs), level: h.level}
}

func (h *stacktraceHandler) WithGroup(name string) slog.Handler {
	return &stacktraceHandler{next: h.next.WithGroup(name), level: h.level}
}

// captureStack captures the call stack, skipping runtime, this wrapper and slog internal frames,
// and keeps only the frames from the business call site on, formatted as "function\n\tfile:line".
func captureStack() string {
	var pcs [64]uintptr
	// skip: runtime.Callers, captureStack, Handle
	n := runtime.Callers(3, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	var b strings.Builder
	for {
		f, more := frames.Next()
		if !strings.HasPrefix(f.Function, "log/slog.") {
			fmt.Fprintf(&b, "%s\n\t%s:%d\n", f.Function, f.File, f.Line)
		}
		if !more {
			break
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
