package mw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"buf.build/go/protovalidate"
	otelmetrics "github.com/go-kratos/kratos/contrib/otel/v3/metrics"
	oteltracing "github.com/go-kratos/kratos/contrib/otel/v3/tracing"
	kerrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/go-kratos/kratos/v3/middleware"
	"github.com/go-kratos/kratos/v3/middleware/circuitbreaker"
	"github.com/go-kratos/kratos/v3/middleware/ratelimit"
	"github.com/go-kratos/kratos/v3/middleware/recovery"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/proto"

	"github.com/go-keel/keel/core/kconfig"
	"github.com/go-keel/keel/internal/errs"
	"github.com/go-keel/keel/middleware/auth"
	"github.com/go-keel/keel/middleware/locale"
	"github.com/go-keel/keel/middleware/logging"
	"github.com/go-keel/keel/middleware/metadata"
	"github.com/go-keel/keel/middleware/sentinel"
	"github.com/go-keel/keel/pkg/redact"
)

// Default returns the recommended server-side middleware chain.
// The Auth slot is a reserved placeholder (pass-through middleware) and is not configured here: the server
// constructors (api.NewHTTPServer / NewGRPCServer) build authentication once from their own Server_Auth config and
// fill it into their own Clone() of the chain; users cannot fill it.
// Sentinel and Locale are NOT included — add via WithSentinel/WithLocale.
//
// It takes no observability parameter: the metrics middleware uses otel's global MeterProvider directly (a process
// facility, see kotel.Setup); when kotel.Setup has not run or otel.enabled=false that global is a harmless no-op
// implementation, so no nil check is needed here.
//
// Each server calls Default once to build its own chain; to derive variants from a shared base chain, Clone() first
// and then customise (the combinators modify the chain in place).
func Default(cfg *kconfig.Config) *Chain {
	chain := empty()
	metrics, merr := serverMetrics(appName(cfg))
	validator, verr := protovalidateFunc()
	// redact.Validate fails startup when a redact annotation cannot take effect; its errors are already rule errors.
	chain.err = errors.Join(merr, verr, redact.Validate())

	chain.add(SlotRecovery, recovery.Recovery())
	chain.add(SlotTracing, oteltracing.Server())
	chain.add(SlotMetrics, metrics)
	// Request body/response body/request headers are not logged by default (to prevent PII leaks); enable with WithLogging as needed.
	chain.add(SlotLogging, logging.Server())
	chain.add(SlotMetadata, metadata.Server())
	chain.reserveAuth()
	chain.add(SlotValidate, validateMiddleware(validator))

	return chain
}

// WithRatelimit inserts adaptive rate limiting (kratos BBR limiter) right
// after SlotRecovery, shedding excess load before tracing/logging costs.
// BBR is self-tuning (CPU + inflight based) and needs no configuration; pass
// ratelimit.Option to customize (e.g. a custom Limiter).
func WithRatelimit(chain *Chain, opts ...ratelimit.Option) {
	chain.InsertAfter(SlotRecovery, ratelimit.Server(opts...), SlotRatelimit)
}

// Minimal returns recovery + logging only.
func Minimal() *Chain {
	return empty().
		add(SlotRecovery, recovery.Recovery()).
		add(SlotLogging, logging.Server())
}

// ClientDefault returns the recommended client-side middleware chain:
// recovery, tracing, metrics, metadata propagation, circuit breaker.
// Auth is NOT included — add it with WithClientAuth.
//
// It takes no observability parameter, for the same reason as Default.
func ClientDefault(cfg *kconfig.Config) *ClientChain {
	metrics, err := clientMetrics(appName(cfg))
	chain := &ClientChain{err: err}
	return chain.
		add(ClientSlotRecovery, recovery.Recovery()).
		add(ClientSlotTracing, oteltracing.Client()).
		add(ClientSlotMetrics, metrics).
		add(ClientSlotMetadata, metadata.Client()).
		add(ClientSlotCircuitBreak, circuitbreaker.Client())
}

// WithLogging replaces the logging slot with logging.Server(opts...), e.g. to also record request and response bodies
// (logging.WithLogRequest(true), logging.WithLogResponse(true)) or headers while investigating, or to skip paths
// (logging.WithSkipPaths).
//
// Proto fields marked (keel.v1.redact).sensitive are blanked to "***" and those marked bulky to
// "[omitted N bytes]", so credentials do not hit disk because bodies are logged, and base64 images do not crowd out
// the whole log line. It is still opt-in: sensitive fields **without the annotation** are logged as is, and non-proto
// payloads have nothing to go by at all; before enabling, make sure the service's contract is fully annotated.
func WithLogging(chain *Chain, opts ...logging.Option) {
	chain.replace(SlotLogging, logging.Server(opts...))
}

// WithTrustedMetadata replaces the metadata slot with the variant that trusts the incoming x-keel-remote-address, for
// services that accept only internal traffic (internal gRPC services, or internal HTTP services behind a trusted
// gateway), so the client IP computed at the entry propagates along the call chain.
//
// By default no server trusts the incoming value. Servers that may receive external traffic must not use this combinator.
func WithTrustedMetadata(chain *Chain) {
	chain.replace(SlotMetadata, metadata.Server(metadata.WithTrustIncoming()))
}

// WithLocale inserts the locale middleware right after SlotMetadata so the
// localizer is available to downstream middleware (e.g. sentinel) and handlers.
// A nil bundle records an error on the chain: every request would otherwise panic on it at serve time.
func WithLocale(chain *Chain, bundle *i18n.Bundle) {
	if bundle == nil {
		chain.err = errors.Join(chain.err, errs.Rules.New("mw.locale-bundle-missing", "mw.WithLocale was given a nil bundle", "pass the go-i18n bundle with the service's message files loaded, or drop the WithLocale call"))
		return
	}
	chain.InsertAfter(SlotMetadata, locale.Server(bundle), SlotLocale)
}

// WithSentinel appends the sentinel error-mapping middleware (runs last, closest
// to the business handler). A nil group, or registration errors recorded by the group (sentinel.Group.Err), are
// recorded on the chain.
func WithSentinel(chain *Chain, sg *sentinel.Group) {
	if sg == nil {
		chain.err = errors.Join(chain.err, errs.Rules.New("mw.sentinel-group-missing", "mw.WithSentinel was given a nil sentinel group", "pass a group built with sentinel.NewGroup, or drop the WithSentinel call"))
		return
	}
	chain.err = errors.Join(chain.err, sg.Err())
	chain.add(SlotSentinel, sg.Middleware())
}

// appName tolerates a nil config so tests can build a chain with Default(nil).
func appName(cfg *kconfig.Config) string {
	if cfg != nil && cfg.GetApp() != nil {
		return cfg.GetApp().GetName()
	}
	return "keel"
}

// meterOf uses otel's global MeterProvider directly (a process facility). When kotel.Setup has not run or
// otel.enabled=false, otel.GetMeterProvider() is itself a harmless no-op implementation, so no nil-check fallback is
// needed.
func meterOf(name string) metric.Meter {
	return otel.GetMeterProvider().Meter(name)
}

// buildMetrics builds the otel metrics middleware; server and client differ only in the metric names and the final
// constructor. It fails only when the meter rejects a constant metric name (practically impossible), and still
// follows the startup rule: it returns an error for the caller to record on the chain, never panics, and first logs a
// line naming which item failed. On failure it returns a pass-through placeholder, so a chain that calls Build()
// without checking Err() does not crash on a nil middleware.
func buildMetrics(
	name, kind, counterName, histName string,
	build func(...otelmetrics.Option) Middleware,
) (Middleware, error) {
	meter := meterOf(name)
	counter, err := otelmetrics.DefaultRequestsCounter(meter, counterName)
	if err != nil {
		slog.Error("mw: create requests counter failed", "kind", kind, "instrument", counterName, "error", err)
		return passthrough, fmt.Errorf("mw: %s requests counter: %w", kind, err)
	}
	seconds, err := otelmetrics.DefaultSecondsHistogram(meter, histName)
	if err != nil {
		slog.Error("mw: create seconds histogram failed", "kind", kind, "instrument", histName, "error", err)
		return passthrough, fmt.Errorf("mw: %s seconds histogram: %w", kind, err)
	}
	return build(
		otelmetrics.WithRequests(counter),
		otelmetrics.WithSeconds(seconds),
	), nil
}

func serverMetrics(name string) (Middleware, error) {
	return buildMetrics(name, "server",
		otelmetrics.DefaultServerRequestsCounterName,
		otelmetrics.DefaultServerSecondsHistogramName,
		otelmetrics.Server)
}

func clientMetrics(name string) (Middleware, error) {
	return buildMetrics(name, "client",
		otelmetrics.DefaultClientRequestsCounterName,
		otelmetrics.DefaultClientSecondsHistogramName,
		otelmetrics.Client)
}

// protovalidateFunc adapts protovalidate to validateMiddleware.
// Initialising without options does not fail in practice; on failure, as with buildMetrics, it logs and returns an error, never panics.
func protovalidateFunc() (func(any) error, error) {
	v, err := protovalidate.New()
	if err != nil {
		slog.Error("mw: create protovalidate validator failed", "error", err)
		return nil, fmt.Errorf("mw: protovalidate: %w", err)
	}
	return func(req any) error {
		if msg, ok := req.(proto.Message); ok {
			return v.Validate(msg)
		}
		return nil
	}, nil
}

// validateMiddleware replaces kratos's built-in validate middleware, which also calls a Validate() method on the
// request (protoc-gen-validate's); Keel validates with protovalidate rules only. The error is what kratos returns:
// 400, reason VALIDATOR, the protovalidate text (field path and rule) as the message, the original error as the cause.
// The message is for developers: clients check input before sending it.
func validateMiddleware(fn func(any) error) Middleware {
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			if err := fn(req); err != nil {
				return nil, kerrors.BadRequest("VALIDATOR", err.Error()).WithCause(err)
			}
			return handler(ctx, req)
		}
	}
}

// WithClientAuth mounts the client authentication middleware in ClientSlotAuth: every outgoing call carries
// "Bearer <token()>". token is called per request.
func WithClientAuth(chain *ClientChain, token func() string) {
	chain.add(ClientSlotAuth, auth.Client(token))
}
