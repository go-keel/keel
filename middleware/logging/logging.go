package logging

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-kratos/kratos/v3/encoding"
	_ "github.com/go-kratos/kratos/v3/encoding/protojson" // registers the "protojson" codec
	"github.com/go-kratos/kratos/v3/errors"
	"github.com/go-kratos/kratos/v3/middleware"
	"github.com/go-kratos/kratos/v3/transport"
	khttp "github.com/go-kratos/kratos/v3/transport/http"
	"google.golang.org/protobuf/proto"

	"github.com/go-keel/keel/pkg/redact"
)

// Server returns the server-side logging middleware.
//
// It logs the details of every request, including:
//   - operation: the request path for HTTP, the RPC method name for gRPC
//   - latency: request duration (seconds)
//   - response_code: response status code
//   - request_headers: request headers (optional, can be skipped)
//   - request_body: request body (optional, can be skipped)
//   - response_body: response body (optional, can be skipped, size limited)
//   - error: error message (if any)
//
// Example:
//
//	middlewares := []middleware.Middleware{
//	    logging.Server(
//	        logging.WithLogRequest(true),
//	        logging.WithLogResponse(true),
//	        logging.WithLogHeaders(true),
//	        logging.WithMaxResponseSize(4096),
//	        logging.WithSkipPaths("/health", "/metrics"),
//	    ),
//	}
func Server(opts ...Option) middleware.Middleware {
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}

	// Build the skip path set for fast lookup
	skipPathSet := make(map[string]struct{})
	for _, p := range o.SkipPaths {
		skipPathSet[p] = struct{}{}
	}

	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req interface{}) (interface{}, error) {
			startTime := time.Now()

			// The transport does not change during the request; fetch it once for the extractors below.
			tr, _ := transport.FromServerContext(ctx)

			// Extract the operation name (route path or method name)
			var operation string
			var component string
			if tr != nil {
				component = tr.Kind().String()

				// HTTP request: get the real route path
				if tr.Kind() == transport.KindHTTP {
					if httpTr, ok := tr.(khttp.Transporter); ok {
						operation = httpTr.Request().URL.Path
					} else {
						operation = tr.Operation()
					}
				} else {
					// gRPC request: use Operation (the RPC method name)
					operation = tr.Operation()
				}
			}

			// Check whether request/response logging is skipped for this path
			_, skip := skipPathSet[operation]

			// Extract request headers
			var headers map[string]string
			if o.LogHeaders && !skip {
				headers = extractHeaders(tr)
			}

			// Extract and cache the request body (HTTP needs special handling)
			var requestBody string
			if o.LogRequest && !skip {
				requestBody = marshalBody(req, o.MaxRequestSize)
			}

			// A streaming request (SSE/WebSocket) produces no completion log until the stream ends;
			// log a start line first so a long-lived connection is not invisible meanwhile.
			if streamKind := detectStream(tr); streamKind != "" && !skip {
				slog.LogAttrs(ctx, slog.LevelInfo, operation,
					slog.String("kind", "server"),
					slog.String("component", component),
					slog.String("operation", operation),
					slog.String("stream", streamKind),
					slog.String("phase", "open"),
				)
			}

			// Run the business logic and intercept the response
			var resp interface{}
			var err error
			var responseBody string

			resp, err = handler(ctx, req)

			// Extract the response (only on success and when it should be logged)
			if err == nil && o.LogResponse && !skip {
				responseBody = marshalBody(resp, o.MaxResponseSize)
			}

			// Determine the log level and status code
			level, code := extractError(tr, err)

			// Build the base log fields
			attrs := []slog.Attr{
				slog.String("kind", "server"),
				slog.String("component", component),
				slog.String("operation", operation), // HTTP: route path; gRPC: RPC method name
				slog.Float64("latency", time.Since(startTime).Seconds()),
				slog.Int("response_code", int(code)),
			}

			// Log request headers (key:value format)
			if o.LogHeaders && !skip && len(headers) > 0 {
				attrs = append(attrs, slog.String("request_headers", formatHeaders(headers)))
			}

			// Log the request body
			if o.LogRequest && !skip && requestBody != "" {
				attrs = append(attrs, slog.String("request_body", requestBody))
			}

			// Log the response body (only when there is no error)
			if o.LogResponse && !skip && err == nil && responseBody != "" {
				attrs = append(attrs, slog.String("response_body", responseBody))
			}

			// Log the error
			if err != nil {
				attrs = append(attrs, slog.String("error", err.Error()))
			}

			// Write the log (ctx variant: both the console extractor and otelslog correlate the trace automatically)
			slog.LogAttrs(ctx, level, operation, attrs...)

			return resp, err
		}
	}
}

// extractError extracts the log level and status code from an error. tr may be nil.
//
// Failures caused by the caller (4xx: not logged in, no permission, bad parameters, not found) log at WARN: they are
// not service faults, and logging them as ERROR would bury real faults and break alerts that count ERRORs; they still
// leave a trace, so a spike reveals credential stuffing, endpoint abuse or a client bug. 5xx and unrecognised errors
// log at ERROR.
func extractError(tr transport.Transporter, err error) (slog.Level, int32) {
	if err == nil {
		// On success HTTP returns 200, gRPC returns 0
		if tr != nil && tr.Kind() == transport.KindHTTP {
			return slog.LevelInfo, 200
		}
		return slog.LevelInfo, 0
	}
	// errors.FromError always returns non-nil for a non-nil err (UnknownCode=500 as fallback).
	code := errors.FromError(err).Code
	if code >= 400 && code < 500 {
		return slog.LevelWarn, code
	}
	return slog.LevelError, code
}

// sensitiveHeaders lists credential headers that must be redacted when logging request headers (compared in lower case).
var sensitiveHeaders = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"cookie":              {},
	"set-cookie":          {},
	"x-api-key":           {},
}

// extractHeaders extracts request headers; credential headers are redacted to [redacted] so that tokens/cookies do
// not reach the logs when LogHeaders is on. tr may be nil.
func extractHeaders(tr transport.Transporter) map[string]string {
	if tr == nil {
		return nil
	}
	header := tr.RequestHeader()
	headers := make(map[string]string)
	// Iterate over all request headers
	for _, k := range header.Keys() {
		if _, sensitive := sensitiveHeaders[strings.ToLower(k)]; sensitive {
			headers[k] = "[redacted]"
			continue
		}
		headers[k] = header.Get(k)
	}
	return headers
}

// formatHeaders formats request headers as plain key:value text (newline separated).
func formatHeaders(headers map[string]string) string {
	if len(headers) == 0 {
		return ""
	}

	var result string
	for k, v := range headers {
		if result != "" {
			result += "\n"
		}
		result += k + ":" + v
	}
	return result
}

// marshalBody serialises a request/response body consistently with the Keel HTTP server's default encoding:
// proto messages use protojson semantics, everything else stdlib json.
//
// Proto messages go through redact first: fields marked (keel.v1.redact).sensitive are "***" in the log. Non-proto
// payloads have no annotation to go by and are serialised as is; redaction on that path is the caller's
// responsibility.
func marshalBody(v interface{}, maxSize int) string {
	if v == nil {
		return ""
	}
	if isBinary(v) {
		return "[binary]"
	}
	codecName := "json"
	if pm, ok := v.(proto.Message); ok {
		codecName = "protojson"
		v = redact.Redact(pm)
	}
	data, err := encoding.GetCodec(codecName).Marshal(v)
	if err != nil {
		return fmt.Sprintf("[%T]", v)
	}
	return sizeLimited(data, maxSize)
}

// sizeLimited returns the data; above maxSize it truncates at a UTF-8 boundary and appends a marker, keeping the
// prefix for debugging (rather than dropping it entirely).
func sizeLimited(data []byte, maxSize int) string {
	if maxSize > 0 && len(data) > maxSize {
		return truncateUTF8(string(data), maxSize) + "...[truncated]"
	}
	return string(data)
}

// isBinary reports whether the value is a binary type.
func isBinary(v interface{}) bool {
	switch v.(type) {
	case []byte:
		return true
	}
	return false
}

// detectStream recognises a contract streaming request: WebSocket by the Upgrade header, SSE by Accept.
// It returns "websocket" / "sse" / "" (not streaming). tr may be nil.
func detectStream(tr transport.Transporter) string {
	if tr == nil || tr.Kind() != transport.KindHTTP {
		return ""
	}
	header := tr.RequestHeader()
	if strings.EqualFold(header.Get("Upgrade"), "websocket") {
		return "websocket"
	}
	if strings.Contains(header.Get("Accept"), "text/event-stream") {
		return "sse"
	}
	return ""
}

// truncateUTF8 cuts s to at most maxBytes bytes on a rune boundary, so a partial rune is never logged.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	n := 0
	for i := range s {
		if i > maxBytes {
			break
		}
		n = i
	}
	return s[:n]
}
