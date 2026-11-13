// In-package: checks the console handler's record layout and trace extraction, which SetupLogger does not expose.

package keel

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	kratoslog "github.com/go-kratos/kratos/v3/log"
	"go.opentelemetry.io/otel/trace"
)

// Trace correlation works in json format too: one JSON object per line, with trace_id and source, so the log
// platform parses fields without regexes.
func TestConsoleHandler_JSONCarriesTraceAndSource(t *testing.T) {
	tid, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	sid, _ := trace.SpanIDFromHex("0102030405060708")
	ctx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid}))

	var buf bytes.Buffer
	slog.New(consoleHandler(&buf, slog.LevelInfo, kratoslog.FormatJSON)).InfoContext(ctx, "hello")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not one JSON object: %v\n%s", err, buf.String())
	}
	if rec["trace_id"] != tid.String() {
		t.Errorf("trace_id = %v, want %s", rec["trace_id"], tid)
	}
	if _, ok := rec[slog.SourceKey]; !ok {
		t.Errorf("missing %q field: %v", slog.SourceKey, rec)
	}
}

func TestTraceExtractor_NoSpan(t *testing.T) {
	if attrs := traceExtractor(context.Background()); len(attrs) != 0 {
		t.Errorf("no span in ctx should yield no attrs, got %v", attrs)
	}
}

func TestTraceExtractor_WithSpan(t *testing.T) {
	tid, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	sid, _ := trace.SpanIDFromHex("0102030405060708")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	attrs := traceExtractor(ctx)
	if len(attrs) != 2 {
		t.Fatalf("want 2 attrs (trace_id, span_id), got %d", len(attrs))
	}
	if attrs[0].Key != "trace_id" || attrs[0].Value.String() != tid.String() {
		t.Errorf("trace_id attr mismatch: %v", attrs[0])
	}
	if attrs[1].Key != "span_id" || attrs[1].Value.String() != sid.String() {
		t.Errorf("span_id attr mismatch: %v", attrs[1])
	}
}
