package logging

// options holds the configuration of the logging middleware; set it only through the WithXxx options.
//
// Request body, response body and request headers are not logged by default: bodies may contain sensitive data (PII,
// credentials); enable them explicitly per service when needed.
type options struct {
	// LogRequest controls whether the request body is logged; default false.
	// The request body is logged in the "request_body" field.
	LogRequest bool

	// LogResponse controls whether the response body is logged; default false.
	// The response body is logged in the "response_body" field.
	LogResponse bool

	// LogHeaders controls whether request headers are logged; default false.
	// Request headers are logged in the "request_headers" field; credential headers
	// (Authorization/Cookie and the like) are redacted to [redacted] automatically.
	LogHeaders bool

	// MaxRequestSize is the maximum logged request body size in bytes; default 4096.
	// The excess is truncated at a UTF-8 boundary and "...[truncated]" is appended.
	// Set to 0 for no limit.
	MaxRequestSize int

	// MaxResponseSize is the maximum logged response body size in bytes; default 4096.
	// The excess is truncated at a UTF-8 boundary and "...[truncated]" is appended.
	// Set to 0 for no limit.
	MaxResponseSize int

	// SkipPaths lists paths whose request/response are not logged (exact match).
	// Logs for these paths contain no request, response or headers fields.
	SkipPaths []string
}

// Option is the configuration function type.
type Option func(*options)

// WithLogRequest sets whether the request body is logged (field "request_body"); default false.
func WithLogRequest(v bool) Option {
	return func(o *options) {
		o.LogRequest = v
	}
}

// WithLogResponse sets whether the response body is logged (field "response_body"); default false.
func WithLogResponse(v bool) Option {
	return func(o *options) {
		o.LogResponse = v
	}
}

// WithLogHeaders sets whether request headers are logged (field "request_headers"); default false. Credential
// headers (Authorization, Cookie and the like) are redacted to [redacted].
func WithLogHeaders(v bool) Option {
	return func(o *options) {
		o.LogHeaders = v
	}
}

// WithMaxRequestSize sets the maximum logged request body size in bytes; default 4096.
// The excess is truncated at a UTF-8 boundary and "...[truncated]" is appended.
// Set to 0 for no limit.
func WithMaxRequestSize(size int) Option {
	return func(o *options) {
		o.MaxRequestSize = size
	}
}

// WithMaxResponseSize sets the maximum logged response body size in bytes; default 4096.
// The excess is truncated at a UTF-8 boundary and "...[truncated]" is appended.
// Set to 0 for no limit.
func WithMaxResponseSize(size int) Option {
	return func(o *options) {
		o.MaxResponseSize = size
	}
}

// WithSkipPaths sets the paths whose request/response are not logged.
// Logs for these paths contain no request, response or headers fields.
func WithSkipPaths(paths ...string) Option {
	return func(o *options) {
		o.SkipPaths = append(o.SkipPaths, paths...)
	}
}

// defaultOptions returns the default configuration.
func defaultOptions() *options {
	return &options{
		LogRequest:      false,
		LogResponse:     false,
		LogHeaders:      false,
		MaxRequestSize:  4096, // 4KB
		MaxResponseSize: 4096, // 4KB
		SkipPaths:       nil,
	}
}
