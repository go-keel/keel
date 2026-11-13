// Package msgtrace gathers the tracing logic shared by events (core/knats) and tasks (core/kasynq): "start a span and
// write the message headers when sending, continue the trace when receiving, finish according to the result". Both
// sides share the same failure-marking semantics: every failed handling is marked failed, retries and final give-ups
// alike.
//
// It holds only the three actions send, receive and finish by result. Each side's panic recovery (their policies on
// retry limits, whether the archive queue is affected and so on already differ) is not here; see safeHandle in
// core/knats/consumer.go and panicRecoveryMiddleware in core/kasynq/server.go, deliberately not merged.
package msgtrace

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

// StartSend starts a producer span and injects the (new span's) trace context into carrier, from which StartReceive
// on the receiving side continues the trace.
//
// system is used both as the messaging.system attribute value and as the tracer's instrumentation scope name
// ("nats", "asynq"), so both sides use one system identifier instead of naming it separately. destination maps to
// messaging.destination.name: the full subject for events, the queue name for tasks. extra holds the caller-specific
// attributes (task name, consumer name and so on), appended as given.
func StartSend(ctx context.Context, system, spanName, destination string, carrier propagation.TextMapCarrier, extra ...attribute.KeyValue) (context.Context, trace.Span) {
	attrs := append([]attribute.KeyValue{
		semconv.MessagingSystemKey.String(system),
		semconv.MessagingDestinationNameKey.String(destination),
		semconv.MessagingOperationPublish,
	}, extra...)
	ctx, span := otel.Tracer(system).Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(attrs...),
	)
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return ctx, span
}

// StartReceive continues the sending side's trace from carrier and starts a consumer span.
//
// messaging.operation is fixed to deliver: the knats Consume callback and the kasynq ServeMux dispatch are both push
// models (the server calls the handler), not an active pull by business code (receive in semconv v1.24.0 means the
// pull model only).
func StartReceive(ctx context.Context, system, spanName, destination string, carrier propagation.TextMapCarrier, extra ...attribute.KeyValue) (context.Context, trace.Span) {
	ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	attrs := append([]attribute.KeyValue{
		semconv.MessagingSystemKey.String(system),
		semconv.MessagingDestinationNameKey.String(destination),
		semconv.MessagingOperationDeliver,
	}, extra...)
	return otel.Tracer(system).Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attrs...),
	)
}

// MarkFailed marks this send or handling as failed: RecordError + SetStatus.
//
// A failed handling is marked failed, retries and give-ups alike; callers must not call it only on the "final give-up".
// The success path does not call MarkFailed and the span status stays Unset: in OTel semantics Unset and an explicit Ok
// look the same in most backends, so "success" does not deserve its own SetStatus call.
func MarkFailed(span trace.Span, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
