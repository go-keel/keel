// In-package: checks the GORM logger adapter (levels, slow threshold), which kdb installs internally.

package kdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/go-keel/keel/conf"
)

// captureLogs temporarily replaces slog's default output with an in-memory buffer and restores it when the test
// ends.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

// trace runs Trace once and returns the log output and how many times fc was called.
func trace(t *testing.T, l gormlogger.Interface, elapsed time.Duration, err error) (string, int) {
	t.Helper()
	buf := captureLogs(t)
	calls := 0
	fc := func() (string, int64) { calls++; return "SELECT 1", 1 }
	l.Trace(context.Background(), time.Now().Add(-elapsed), fc, err)
	return buf.String(), calls
}

func newLogger(t *testing.T, level string, slow time.Duration) gormlogger.Interface {
	t.Helper()
	c := &conf.Database{LogLevel: level}
	if slow > 0 {
		c.SlowThreshold = durationpb.New(slow)
	}
	return newSlogLogger(c)
}

// Trace's dispatch is kdb's own logic and is asserted case by case; when gorm calls Trace is upstream's business
// and is not tested.
func TestTraceDispatch(t *testing.T) {
	cases := []struct {
		name      string
		level     string
		elapsed   time.Duration
		err       error
		wantLevel string // empty means no output expected
	}{
		{"error logs ERROR", "warn", time.Millisecond, errors.New("boom"), "ERROR"},
		{"record not found is not an error", "warn", time.Millisecond, gorm.ErrRecordNotFound, ""},
		{"canceled request logs DEBUG", "warn", time.Millisecond, fmt.Errorf("query: %w", context.Canceled), "DEBUG"},
		{"deadline exceeded still logs ERROR", "warn", time.Millisecond, context.DeadlineExceeded, "ERROR"},
		{"slow query logs WARN", "warn", 500 * time.Millisecond, nil, "WARN"},
		{"normal query not logged at warn level", "warn", time.Millisecond, nil, ""},
		{"normal query logs INFO at info level", "info", time.Millisecond, nil, "INFO"},
		{"silent logs nothing", "silent", 500 * time.Millisecond, errors.New("boom"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, calls := trace(t, newLogger(t, tc.level, 0), tc.elapsed, tc.err)
			if tc.wantLevel == "" {
				if out != "" {
					t.Fatalf("want no log, got: %s", out)
				}
				// fc runs db.Explain internally to build the full SQL, so it must not be called when nothing is logged.
				if calls != 0 {
					t.Fatalf("fc called %d times without logging; it must stay inside the branches", calls)
				}
				return
			}
			if !strings.Contains(out, "level="+tc.wantLevel) {
				t.Fatalf("want level=%s, got: %s", tc.wantLevel, out)
			}
			if calls != 1 {
				t.Fatalf("fc called %d times, want exactly 1", calls)
			}
		})
	}
}

// The threshold comes from database.<name>.slow_threshold; 200ms is only the default.
func TestSlowThresholdFromConfig(t *testing.T) {
	out, _ := trace(t, newLogger(t, "warn", time.Second), 500*time.Millisecond, nil)
	if out != "" {
		t.Fatalf("500ms does not exceed the configured 1s threshold, no slow query should be logged: %s", out)
	}
	out, _ = trace(t, newLogger(t, "warn", 100*time.Millisecond), 500*time.Millisecond, nil)
	if !strings.Contains(out, "threshold_ms=100") {
		t.Fatalf("want threshold_ms=100 from config, got: %s", out)
	}
}

// 0 and omission mean the default; only a positive value overrides it (negative values are rejected by conf.proto).
func TestDurationOrUsesDefaultUnlessPositive(t *testing.T) {
	const def = 5 * time.Minute
	cases := []struct {
		name string
		d    *durationpb.Duration
		want time.Duration
	}{
		{"unset", nil, def},
		{"zero", durationpb.New(0), def},
		{"positive", durationpb.New(time.Second), time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := durationOr(tc.d, def); got != tc.want {
				t.Fatalf("durationOr = %v, want %v", got, tc.want)
			}
		})
	}
}

// slow_threshold 0 keeps the 200ms default rather than logging every query as slow.
func TestSlowThresholdZeroUsesDefault(t *testing.T) {
	l := newSlogLogger(&conf.Database{SlowThreshold: durationpb.New(0)}).(*slogLogger)
	if l.slowThreshold != defaultSlowThreshold {
		t.Fatalf("slowThreshold = %v, want %v", l.slowThreshold, defaultSlowThreshold)
	}
}
