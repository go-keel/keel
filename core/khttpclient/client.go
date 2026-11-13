// Package khttpclient provides the HTTP client for calling Keel services (built on Resty v2).
//
// Responses are modelled in one of two ways: a struct hand-written from the other side's HTTP+JSON contract goes
// through SetResult; a caller that imports the other side's pb package and wants the generated type uses
// SetResult(ProtoResult(&msg)), decoded with protojson. gRPC remains the first choice between Keel services. The fixed
// error envelope is restored automatically, and business errors are checked by reason string through IsReason /
// AsError (callers need not touch kratos errors); request logs, trace/metrics and HTTP/2 (including cleartext h2c)
// work out of the box. There is no configuration: the endpoint and other parameters are passed in dynamically by the
// user, who also caches the returned client.
//
// The client deliberately does not propagate x-keel-* metadata (metadata.Client() on the gRPC side does): x-keel-*
// is a convention between internal services, and internal calls go over gRPC; this client does not assume it sits
// inside the trust domain, so HTTP is treated as untrusted and any header to send is set explicitly by the caller.
// This is by design, not an omission; do not "fix" it. The trust model is in docs/metadata.md "Trust model".
package khttpclient

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/go-resty/resty/v2"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/go-keel/keel/internal/errs"
)

// defaultTimeout is the default whole-request timeout; resty has no timeout by default, which is unacceptable for
// service-to-service calls.
const defaultTimeout = 5 * time.Second

// defaultBodyLimit is the default limit on reading a response body; resty has no limit by default, so one runaway
// upstream (an unpaged list, a hijacked gateway) could blow up the whole process, whereas failing past the limit kills
// only one call. It is 2MB: a JSON response to an API call should never reach that size, and when it does, something
// that should be paged is not. Large responses and downloads are outside this component's job (it only makes JSON API
// calls); do not raise this value.
const defaultBodyLimit = 2 << 20 // 2MB

type options struct {
	h2c         bool
	timeout     time.Duration
	tlsConfig   *tls.Config
	serviceName string
	bodyLimit   int
}

type Option func(*options)

// WithH2C makes a cleartext endpoint use HTTP/2 directly (prior-knowledge h2c), for calls between internal services;
// the Keel HTTP server accepts h2c by default. It applies only to http:// endpoints.
func WithH2C() Option { return func(o *options) { o.h2c = true } }

// WithTimeout sets the whole-request timeout; 0 means no timeout. The default is 5s.
func WithTimeout(d time.Duration) Option { return func(o *options) { o.timeout = d } }

// WithTLSConfig sets a custom TLS configuration for https endpoints.
func WithTLSConfig(c *tls.Config) Option { return func(o *options) { o.tlsConfig = c } }

// WithServiceName sets the peer service name, which goes into the request log fields and the trace span attributes;
// it defaults to the endpoint's host.
func WithServiceName(name string) Option { return func(o *options) { o.serviceName = name } }

// WithResponseBodyLimit sets the limit on reading a response body; exceeding it is an error. The default is 2MB.
//
// This Option accepts only positive numbers: assembly offers no way to "turn the limit off", because the case that
// needs it is receiving large responses, which is outside this client's job (see defaultBodyLimit). A JSON API that
// temporarily exceeds 2MB should set a specific value that is large enough.
//
// This is not a hard constraint: New returns the native *resty.Client, so the caller can always override it with
// cli.SetResponseBodyLimit(0), or per request with R().SetResponseBodyLimit(0) (resty treats a non-positive number as
// unlimited). That is the inherent cost of the native escape hatch: once overridden, the OOM risk belongs to the
// caller.
func WithResponseBodyLimit(n int) Option { return func(o *options) { o.bodyLimit = n } }

// New creates a resty client pointing at a Keel service. It returns the native *resty.Client; the caller passes the
// context with R().SetContext(ctx), and cleanup closes idle connections. trace/metrics are always attached: when no
// provider is given explicitly, otelhttp uses otel's global TracerProvider/MeterProvider (a process facility, see
// kotel.Setup), and when otel is not enabled those two globals are themselves harmless no-op implementations, so
// there is no need to check for them here and skip.
func New(endpoint string, opts ...Option) (*resty.Client, func(), error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, nil, errs.Rules.New("khttpclient.endpoint-invalid", fmt.Sprintf("khttpclient.New was given endpoint %q, which is not an absolute http:// or https:// URL with a host", endpoint), "pass the base URL of the service, such as \"http://user-svc:8000\"")
	}
	o := &options{timeout: defaultTimeout, serviceName: u.Host, bodyLimit: defaultBodyLimit}
	for _, opt := range opts {
		opt(o)
	}
	if o.h2c && u.Scheme != "http" {
		return nil, nil, errs.Rules.New("khttpclient.h2c-scheme", fmt.Sprintf("WithH2C is set but the endpoint %q is not http://", endpoint), "use an http:// endpoint with WithH2C, or drop WithH2C for https:// (HTTP/2 over TLS is negotiated on its own)")
	}
	if o.bodyLimit <= 0 {
		return nil, nil, errs.Rules.New("khttpclient.body-limit", fmt.Sprintf("WithResponseBodyLimit was given %d", o.bodyLimit),
			"pass a positive byte limit large enough for the expected responses; large downloads belong to another channel")
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = o.tlsConfig
	var protocols http.Protocols
	if o.h2c {
		// Cleartext prior-knowledge HTTP/2: HTTP1 must not be enabled as well, or http:// still uses 1.1.
		protocols.SetHTTP2(true)
		protocols.SetUnencryptedHTTP2(true)
	} else {
		protocols.SetHTTP1(true)
		protocols.SetHTTP2(true) // https negotiates through ALPN on its own
	}
	tr.Protocols = &protocols

	// otelhttp provides spans, latency metrics and W3C traceparent injection; the Keel server's tracing middleware
	// continues the trace from it. WithTracerProvider/WithMeterProvider are not passed explicitly: without them
	// otelhttp already defaults to otel's global Providers (newConfig in otelhttp@v0.70.0 config.go), so always
	// attaching needs no "refuse to fall back to the global" plumbing; that would be exactly the nil-check logic to
	// avoid, since the globals are either fully configured or entirely no-op.
	rt := otelhttp.NewTransport(tr,
		otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{})),
		otelhttp.WithSpanOptions(trace.WithAttributes(
			attribute.String("peer.service", o.serviceName))),
	)

	cli := resty.New().
		SetTransport(rt).
		SetBaseURL(endpoint).
		SetTimeout(o.timeout).
		SetResponseBodyLimit(o.bodyLimit).
		// Accept must match the types checkResponseType lets through: advertising only application/json would make a
		// peer that does content negotiation answer 406 on plain-text endpoints (health checks and the like), which
		// are exactly a type this client claims to accept. The q value says JSON first, text second.
		SetHeader("Accept", "application/json, text/*;q=0.9").
		// Result decoding is done by decodeResult in the hook chain; see skipUnmarshal. A caller that calls
		// SetJSONUnmarshaler again hands decoding back to resty's up-front parsing, which on failure skips the whole
		// hook chain again (no status code, no contract error); that is the cost of the native escape hatch.
		SetJSONUnmarshaler(skipUnmarshal)
	cli.OnBeforeRequest(markLoggable)
	// The order is the semantics: checkResponseType and decodeResult run before logResponse, so a response judged an
	// error is logged as ERROR (with the status code) through OnError instead of first leaving a status=200 INFO;
	// logResponse runs before decodeError because resty's response middleware stops at the first error, so the log
	// is written before the business error is restored. decodeResult and decodeError handle 2xx and non-2xx
	// respectively and never overlap.
	cli.OnAfterResponse(checkResponseType)
	cli.OnAfterResponse(decodeResult)
	cli.OnAfterResponse(logResponse(o.serviceName))
	cli.OnAfterResponse(decodeError)
	cli.OnError(logError(o.serviceName))
	return cli, tr.CloseIdleConnections, nil
}
