package logging

// Options defines the configuration options of the logging middleware.
//
// Request body, response body and request headers are not logged by default: bodies may contain sensitive data (PII,
// credentials); enable them explicitly per service when needed.
type Options struct {
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
type Option func(*Options)

// WithLogRequest sets whether the request body is logged.
func WithLogRequest(v bool) Option {
	return func(o *Options) {
		o.LogRequest = v
	}
}

// WithLogResponse sets whether the response body is logged.
func WithLogResponse(v bool) Option {
	return func(o *Options) {
		o.LogResponse = v
	}
}

// WithLogHeaders sets whether request headers are logged.
func WithLogHeaders(v bool) Option {
	return func(o *Options) {
		o.LogHeaders = v
	}
}

// WithMaxRequestSize sets the maximum logged request body size in bytes.
// The excess is truncated at a UTF-8 boundary and "...[truncated]" is appended.
// Set to 0 for no limit.
func WithMaxRequestSize(size int) Option {
	return func(o *Options) {
		o.MaxRequestSize = size
	}
}

// WithMaxResponseSize sets the maximum logged response body size in bytes.
// The excess is truncated at a UTF-8 boundary and "...[truncated]" is appended.
// Set to 0 for no limit.
func WithMaxResponseSize(size int) Option {
	return func(o *Options) {
		o.MaxResponseSize = size
	}
}

// WithSkipPaths sets the paths whose request/response are not logged.
// Logs for these paths contain no request, response or headers fields.
func WithSkipPaths(paths ...string) Option {
	return func(o *Options) {
		o.SkipPaths = append(o.SkipPaths, paths...)
	}
}

// defaultOptions returns the default configuration.
func defaultOptions() *Options {
	return &Options{
		LogRequest:      false,
		LogResponse:     false,
		LogHeaders:      false,
		MaxRequestSize:  4096, // 4KB
		MaxResponseSize: 4096, // 4KB
		SkipPaths:       nil,
	}
}
