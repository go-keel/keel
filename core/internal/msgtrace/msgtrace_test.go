package msgtrace_test

import (
	"context"
	"errors"
	"testing"

	"github.com/go-keel/keel/core/internal/msgtrace"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// setupRecording temporarily replaces the global TracerProvider/TextMapPropagator with the real SDK implementations
// and restores them afterwards: tests that assert spans must not pollute other tests in the same process, nor run in
// parallel (a process has only one set of globals).
func setupRecording(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	oldTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(oldTP)
	})

	oldProp := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() { otel.SetTextMapPropagator(oldProp) })

	return exp
}

func attrValue(attrs []attribute.KeyValue, key string) (string, bool) {
	for _, a := range attrs {
		if string(a.Key) == key {
			return a.Value.AsString(), true
		}
	}
	return "", false
}

// Events (nats) and tasks (asynq) share the same send/receive logic: this test is parameterised on system and runs
// twice, covering injection and extraction for both sides at once instead of repeating these assertions in knats and
// kasynq.
func TestStartSendAndReceive_SharedAcrossSystems(t *testing.T) {
	for _, system := range []string{"nats", "asynq"} {
		t.Run(system, func(t *testing.T) {
			exp := setupRecording(t)

			carrier := propagation.MapCarrier{}
			sendCtx, sendSpan := msgtrace.StartSend(context.Background(), system, "test.send", "dest-1", carrier,
				attribute.String("extra.key", "v"))
			sendSpan.End()

			if carrier["traceparent"] == "" {
				t.Fatal("StartSend did not inject traceparent into carrier")
			}

			recvCtx, recvSpan := msgtrace.StartReceive(context.Background(), system, "test.receive", "dest-1", carrier)
			recvSpan.End()

			wantTrace := trace.SpanContextFromContext(sendCtx).TraceID()
			gotTrace := trace.SpanContextFromContext(recvCtx).TraceID()
			if gotTrace != wantTrace {
				t.Errorf("receive trace = %s, want %s (send)", gotTrace, wantTrace)
			}

			spans := exp.GetSpans()
			if len(spans) != 2 {
				t.Fatalf("got %d spans, want 2", len(spans))
			}
			send, recv := spans[0], spans[1]

			if send.SpanKind != trace.SpanKindProducer {
				t.Errorf("send span kind = %v, want Producer", send.SpanKind)
			}
			if recv.SpanKind != trace.SpanKindConsumer {
				t.Errorf("receive span kind = %v, want Consumer", recv.SpanKind)
			}
			if recv.Parent.TraceID() != send.SpanContext.TraceID() {
				t.Errorf("receive span parent trace = %s, want %s", recv.Parent.TraceID(), send.SpanContext.TraceID())
			}

			for _, tc := range []struct {
				attrs []attribute.KeyValue
				op    string
			}{
				{send.Attributes, "publish"},
				{recv.Attributes, "deliver"},
			} {
				if v, ok := attrValue(tc.attrs, "messaging.system"); !ok || v != system {
					t.Errorf("messaging.system = %q, %v, want %q", v, ok, system)
				}
				if v, ok := attrValue(tc.attrs, "messaging.destination.name"); !ok || v != "dest-1" {
					t.Errorf("messaging.destination.name = %q, %v, want %q", v, ok, "dest-1")
				}
				if v, ok := attrValue(tc.attrs, "messaging.operation"); !ok || v != tc.op {
					t.Errorf("messaging.operation = %q, %v, want %q", v, ok, tc.op)
				}
			}
			if v, ok := attrValue(send.Attributes, "extra.key"); !ok || v != "v" {
				t.Errorf("extra attribute not carried through StartSend: %q, %v", v, ok)
			}
		})
	}
}

// Failure is marked, success is not: MarkFailed is called only when the caller has decided the handling failed, and a
// span it was not called on must stay Unset.
func TestMarkFailed_SetsErrorStatusAndRecordsError(t *testing.T) {
	exp := setupRecording(t)
	_, span := msgtrace.StartSend(context.Background(), "nats", "test.send", "dest", propagation.MapCarrier{})
	msgtrace.MarkFailed(span, errors.New("boom"))
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Status.Code != codes.Error {
		t.Errorf("status = %v, want Error", spans[0].Status.Code)
	}
	if len(spans[0].Events) == 0 {
		t.Error("MarkFailed should record the error as a span event")
	}
}

func TestStartSend_WithoutMarkFailed_StaysUnset(t *testing.T) {
	exp := setupRecording(t)
	_, span := msgtrace.StartSend(context.Background(), "nats", "test.send", "dest", propagation.MapCarrier{})
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Status.Code != codes.Unset {
		t.Errorf("status = %v, want Unset (success path must not mark failed)", spans[0].Status.Code)
	}
}
