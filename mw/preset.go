package mw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"buf.build/go/protovalidate"
	otelmetrics "github.com/go-kratos/kratos/contrib/otel/v3/metrics"
	oteltracing "github.com/go-kratos/kratos/contrib/otel/v3/tracing"
	kerrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/go-kratos/kratos/v3/middleware"
	"github.com/go-kratos/kratos/v3/middleware/circuitbreaker"
	"github.com/go-kratos/kratos/v3/middleware/ratelimit"
	"github.com/go-kratos/kratos/v3/middleware/recovery"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/go-keel/keel/conf"
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
func Default(cfg *conf.Config) *Chain {
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
func ClientDefault(cfg *conf.Config) *ClientChain {
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
// A nil bundle (or inner i18n.Bundle) records an error on the chain: every request would otherwise panic on it at
// serve time.
func WithLocale(chain *Chain, bundle *locale.Bundle) {
	if bundle == nil || bundle.Bundle == nil {
		chain.err = errors.Join(chain.err, errs.Rules.New("mw.locale-bundle-missing", "mw.WithLocale was given a nil locale bundle", "pass the *locale.Bundle built by locale.NewBundle, or drop the WithLocale call"))
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

func appName(cfg *conf.Config) string {
	if cfg != nil && cfg.GetApp() != nil {
		return cfg.GetApp().GetName()
	}
	return "keel"
}

// meterOf uses otel's global MeterProvider directly (a process facility). When kotel.Setup has not run or
// otel.enabled=false, otel.GetMeterProvider() is itself a harmless no-op implementation, so no nil-check fallback is
// needed; this is exactly the kind of nil check ADR keel-0004 requires removing.
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

// protovalidateFunc adapts protovalidate to the kratos validate middleware.
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

// validateMessages are the sentences users read when validation fails: they say which category is wrong, without the
// field name or the rule text (those are for developers and stay in the cause for the logs). Basic field-format
// validation should not require every business repo to supply its own localisation, so the framework ships en / zh /
// ja; other languages fall back to English.
var validateMessages = map[string]map[string]string{
	"length": {
		"en": "Some input is too short or too long. Please correct it and try again.",
		"zh": "有输入内容的长度不符合要求，请修改后重试。",
		"ja": "入力内容の長さが条件に合っていません。修正してもう一度お試しください。",
	},
	"range": {
		"en": "Some input is outside the allowed range. Please correct it and try again.",
		"zh": "有输入内容超出了允许的范围，请修改后重试。",
		"ja": "入力内容が許可された範囲外です。修正してもう一度お試しください。",
	},
	"format": {
		"en": "Some input has the wrong format. Please correct it and try again.",
		"zh": "有输入内容的格式不正确，请修改后重试。",
		"ja": "入力内容の形式が正しくありません。修正してもう一度お試しください。",
	},
	"required": {
		"en": "Some required input is missing. Please fill it in and try again.",
		"zh": "有必填内容没有填写，请补全后重试。",
		"ja": "必須項目が入力されていません。入力してもう一度お試しください。",
	},
	"other": {
		"en": "The request is not valid. Please check your input and try again.",
		"zh": "请求内容不符合要求，请检查后重试。",
		"ja": "リクエストの内容が正しくありません。入力を確認してもう一度お試しください。",
	},
}

// validateFamily classifies by protovalidate rule ID. Standard rule IDs look like "string.max_len", "int32.gte",
// "string.email", "required"; range rules bounded at both ends contain an underscore ("int32.gte_lte",
// "int32.gt_lte_exclusive"); rule IDs from Keel itself or written by the business go to other.
func validateFamily(err error) string {
	var ve *protovalidate.ValidationError
	if !errors.As(err, &ve) || len(ve.Violations) == 0 || ve.Violations[0].Proto == nil {
		return "other"
	}
	v := ve.Violations[0]
	id := v.Proto.GetRuleId()
	switch {
	case id == "required":
		return "required"
	case hasRuleSuffix(id, "min_len", "min_bytes", "min_items", "min_pairs") && emptyValue(v.FieldValue):
		// An empty value violating "at least N" means not filled in, not a wrong length.
		return "required"
	case hasRuleSuffix(id, "min_len", "max_len", "len", "min_bytes", "max_bytes", "len_bytes", "min_items", "max_items", "min_pairs", "max_pairs"):
		return "length"
	case isRangeRule(id) || hasRuleSuffix(id, "const", "in", "not_in", "defined_only"):
		return "range"
	case hasRuleSuffix(id, "email", "hostname", "ip", "ipv4", "ipv6", "uri", "uri_ref", "address", "uuid", "tuuid", "pattern", "prefix", "suffix", "contains", "not_contains"):
		return "format"
	}
	return "other"
}

// isRangeRule reports whether the rule name consists only of gt / gte / lt / lte (plus the exclusive suffix of ranges), such as "int32.gte_lte".
func isRangeRule(id string) bool {
	_, last, ok := strings.Cut(id, ".")
	if !ok || last == "" {
		return false
	}
	for _, part := range strings.Split(last, "_") {
		switch part {
		case "gt", "gte", "lt", "lte", "exclusive":
		default:
			return false
		}
	}
	return true
}

// emptyValue reports whether a string is empty or a list or map has no elements. An invalid value (field not obtained) is not empty.
func emptyValue(v protoreflect.Value) bool {
	if !v.IsValid() {
		return false
	}
	switch x := v.Interface().(type) {
	case string:
		return x == ""
	case []byte:
		return len(x) == 0
	case protoreflect.List:
		return x.Len() == 0
	case protoreflect.Map:
		return x.Len() == 0
	}
	return false
}

func hasRuleSuffix(id string, names ...string) bool {
	_, last, _ := strings.Cut(id, ".")
	for _, n := range names {
		if last == n || strings.HasPrefix(last, n+".") {
			return true
		}
	}
	return false
}

// validateLang picks the first supported language from Accept-Language: in order of appearance, ignoring q weights
// (browsers already sort by preference); if none is supported it falls back to English.
func validateLang(header string) string {
	for _, part := range strings.Split(header, ",") {
		tag, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		primary, _, _ := strings.Cut(strings.ToLower(tag), "-")
		switch primary {
		case "zh", "ja", "en":
			return primary
		}
	}
	return "en"
}

// validateMiddleware replaces kratos's built-in validate middleware. The error shape is unchanged (400, reason
// VALIDATOR, cause keeps the original protovalidate error; sentinel and the frontend's "check by reason" depend on
// it); only the message is no longer the validation library's English technical text but one plain sentence
// localised by Accept-Language. Old-style requests with their own Validate() method also go through here and all
// map to other.
func validateMiddleware(fn func(any) error) Middleware {
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			var err error
			if v, ok := req.(interface{ Validate() error }); ok {
				err = v.Validate()
			}
			if err == nil {
				err = fn(req)
			}
			if err != nil {
				msg := validateMessages[validateFamily(err)][validateLang(metadata.GetAcceptLanguage(ctx))]
				return nil, kerrors.BadRequest("VALIDATOR", msg).WithCause(err)
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
