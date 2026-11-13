package api

// Tools for type 2 (raw http.Handler, bypassing the middleware chain):
// SSE / file download / WebSocket. Used together with StreamContext.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/go-keel/keel/internal/errs"
)

// Server returns the underlying HTTP server, for raw routes that need full control of authentication (for example
// webhook signature checks: the signature itself is the credential, so RequireLogin/Anonymous would be redundant).
// **Routes mounted from here skip Keel's authentication check**: this is an explicitly declared escape hatch, not a
// third way of "forgetting to wrap"; framework-managed authentication always goes through RequireLogin() / Anonymous().
func (sc *StreamContext) Server() *HTTPServer { return sc.server }

// Filter mounts a filter that runs before routing and applies to every request on this server: contract endpoints,
// raw routes and the framework's liveness check path included, and requests it does not handle must be passed to
// next unchanged. It is for logic that must run before the request is decoded, such as verifying a signature over
// the raw body: a contract endpoint's handler decodes first and then enters the middleware chain, so middleware on
// the chain never sees the raw bytes.
//
// The framework guarantees it runs after the client IP is computed and before routing, so
// metadata.GetRemoteIP(r.Context()) inside it returns the address computed with this port's client_ip config. Do not
// instead wrap the server's whole Handler after NewHTTPServer returns: that runs one step before the client IP is
// computed and cannot get the address.
//
// **This path loses every protection of the middleware chain**: authentication, logging, tracing, panic recovery and
// error mapping have not run yet, and responses the filter writes itself skip sentinel's error mapping. Multiple
// calls nest from outside in, in call order; a nil f records a construction-time error.
func (sc *StreamContext) Filter(f func(http.Handler) http.Handler) {
	if f == nil {
		sc.err = errors.Join(sc.err, errs.Rules.New("api.stream-filter-nil", "StreamContext.Filter was called with a nil filter", "pass a non-nil func(http.Handler) http.Handler, or drop the Filter call"))
		return
	}
	sc.filters = append(sc.filters, f)
}

// RequireLogin returns a registration entry: its Handle/HandleFunc/HandlePrefix require a valid credential on the
// request; a missing or invalid one writes, via khttp.DefaultErrorEncoder, the same structured error as contract
// endpoints (also distinguishing "missing credential" from "invalid credential").
//
// Calling it when the server has no authentication is itself a config error (no verifier to check with, yet the
// route declares "login required"): it records a construction-time error, returned by NewHTTPServer once all
// streamRegister callbacks have run; the returned entry degrades to pass-through only to avoid a nil dereference,
// which does not mean requests will be correctly rejected.
func (sc *StreamContext) RequireLogin() *StreamRoute {
	wrap := sc.requireLogin
	if wrap == nil {
		sc.err = errors.Join(sc.err, errs.Rules.New("api.require-login-without-auth", "a raw route uses StreamContext.RequireLogin but this server has no auth configured", "enable auth in this server's auth section of the config, or register the route through Anonymous"))
		wrap = func(h http.Handler) http.Handler { return h }
	}
	return &StreamRoute{sc: sc, wrap: wrap}
}

// Anonymous returns a registration entry: its Handle/HandleFunc/HandlePrefix require no credential (missing or
// invalid both pass), but a valid credential on the request is still parsed and the identity written (available via
// auth.FromContext in the handler), consistent with the contract endpoints' anonymous allow-list.
//
// Calling it when the server has no authentication is not an error (anonymous routes need no verifier); the entry
// degrades to pass-through.
func (sc *StreamContext) Anonymous() *StreamRoute {
	wrap := sc.anonymous
	if wrap == nil {
		wrap = func(h http.Handler) http.Handler { return h }
	}
	return &StreamRoute{sc: sc, wrap: wrap}
}

// StreamRoute is the registration entry behind RequireLogin()/Anonymous(); its method set has the same shape as the
// corresponding methods of *HTTPServer, with the authentication wrapper applied automatically.
type StreamRoute struct {
	sc   *StreamContext
	wrap func(http.Handler) http.Handler
}

func (r *StreamRoute) Handle(path string, h http.Handler) {
	if r.reserved(path) {
		return
	}
	r.sc.server.Handle(path, r.wrap(h))
}

func (r *StreamRoute) HandleFunc(path string, h http.HandlerFunc) {
	if r.reserved(path) {
		return
	}
	r.sc.server.HandleFunc(path, r.wrap(h).ServeHTTP)
}

func (r *StreamRoute) HandlePrefix(prefix string, h http.Handler) {
	r.sc.server.HandlePrefix(prefix, r.wrap(h))
}

// reserved rejects paths the framework reserves: the mux does not error on duplicate routes, and the later one is
// silently shadowed by the framework's earlier one. It records a construction-time error, the same exit as
// RequireLogin without authentication. Duplicates among contract endpoints are checked by checkHealthConflict; routes
// mounted directly through the Server() escape hatch cannot be checked.
func (r *StreamRoute) reserved(path string) bool {
	if path != HealthPath {
		return false
	}
	r.sc.err = errors.Join(r.sc.err, errHealthConflict)
	return true
}

// SSEEvent represents a single Server-Sent Event.
type SSEEvent struct {
	ID    string
	Event string
	Data  string
	Retry int // milliseconds; 0 = omit
}

// SSEWriter writes SSE events to the response, flushing after each event.
type SSEWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// NewSSEWriter wraps w. Returns an error if w does not implement http.Flusher.
func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, errs.Rules.New("api.sse-no-flush", fmt.Sprintf("NewSSEWriter was given a %T, which does not implement http.Flusher", w), "pass the http.ResponseWriter the server handed to the handler, or make your wrapper implement Flush (and Unwrap)")
	}
	return &SSEWriter{w: w, flusher: f}, nil
}

// WriteEvent writes one SSE event and flushes immediately.
func (s *SSEWriter) WriteEvent(event SSEEvent) error {
	if event.ID != "" {
		if _, err := fmt.Fprintf(s.w, "id: %s\n", event.ID); err != nil {
			return err
		}
	}
	if event.Event != "" {
		if _, err := fmt.Fprintf(s.w, "event: %s\n", event.Event); err != nil {
			return err
		}
	}
	if event.Retry > 0 {
		if _, err := fmt.Fprintf(s.w, "retry: %d\n", event.Retry); err != nil {
			return err
		}
	}
	// SSE spec: a multi-line payload must prefix every line with "data: ", otherwise a line without a colon is
	// dropped by the client as an unknown field and an empty line is dispatched early as an event boundary.
	for _, line := range strings.Split(event.Data, "\n") {
		if _, err := fmt.Fprintf(s.w, "data: %s\n", line); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(s.w, "\n"); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// SSEHandler returns an http.HandlerFunc that sets SSE headers and runs source
// until it returns or the client disconnects (request context cancellation).
func SSEHandler(source func(ctx context.Context, w *SSEWriter) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		sw, err := NewSSEWriter(w)
		if err != nil {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		if err := source(r.Context(), sw); err != nil &&
			!errors.Is(err, context.Canceled) && r.Context().Err() == nil {
			// A client disconnect is a normal end; other errors are logged (headers already sent, the status code cannot change).
			slog.ErrorContext(r.Context(), "stream: sse source failed",
				"path", r.URL.Path, "error", err)
		}
	}
}

// DownloadOptions configures file download response headers.
type DownloadOptions struct {
	Filename    string
	ContentType string // defaults to application/octet-stream
	Inline      bool   // Content-Disposition: inline vs attachment
}

// ServeContent serves an io.ReadSeeker as a file download. Supports range
// requests and conditional GET via http.ServeContent.
func ServeContent(w http.ResponseWriter, r *http.Request, content io.ReadSeeker, opts DownloadOptions) {
	setDownloadHeaders(w, opts)
	http.ServeContent(w, r, opts.Filename, time.Time{}, content)
}

// ServeReader streams an io.Reader as a file download (no range support).
func ServeReader(w http.ResponseWriter, r *http.Request, reader io.Reader, opts DownloadOptions) {
	setDownloadHeaders(w, opts)
	if _, err := io.Copy(w, reader); err != nil &&
		!errors.Is(err, context.Canceled) && r.Context().Err() == nil {
		// A client aborting the download is normal; other errors are logged (the response has started, the status code cannot change).
		slog.ErrorContext(r.Context(), "stream: download copy failed",
			"filename", opts.Filename, "error", err)
	}
}

func setDownloadHeaders(w http.ResponseWriter, opts DownloadOptions) {
	ct := opts.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)

	disposition := "attachment"
	if opts.Inline {
		disposition = "inline"
	}
	if opts.Filename != "" {
		// mime.FormatMediaType handles quote escaping and non-ASCII (RFC 6266/5987 filename*).
		disposition = mime.FormatMediaType(disposition, map[string]string{"filename": opts.Filename})
	}
	w.Header().Set("Content-Disposition", disposition)
}

// WSOptions configures the raw WebSocket handler.
type WSOptions struct {
	// CheckOrigin overrides the same-origin check; nil uses gorilla's default (same origin only).
	// Provide it explicitly when cross-origin access is needed (check an allow-list; do not blindly return true).
	CheckOrigin func(r *http.Request) bool
	// ReadLimit is the per-message byte limit; 0 means no limit.
	ReadLimit int64
	// CloseTimeout is the write timeout of the closing handshake; default 5s.
	CloseTimeout time.Duration
}

// WebSocketHandler adapts a WebSocket session function into a raw http.HandlerFunc
// (path 3: bypasses the unary middleware chain; wrap it with StreamContext.Auth when authentication is needed).
//
// Contract bidirectional streams should prefer proto bidi-stream RPCs (kratos-generated stubs map them to WebSocket
// automatically and run the full middleware chain); this helper is for free-form frames
// (text protocols, binary forwarding, non-proto message formats dictated by a third party).
//
// The session ends when the handler returns or the client disconnects; a normal return sends CloseNormalClosure,
// an error return sends CloseInternalServerErr (error details are logged only, never sent).
func WebSocketHandler(handler func(ctx context.Context, conn *websocket.Conn) error, opts ...WSOptions) http.HandlerFunc {
	var opt WSOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	if opt.CloseTimeout <= 0 {
		opt.CloseTimeout = 5 * time.Second
	}
	upgrader := websocket.Upgrader{CheckOrigin: opt.CheckOrigin}

	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			// Upgrade has already written the HTTP error response; only log here. Log at WARN, not ERROR: failures are
			// almost always caused by the client (origin rejected 403, bad handshake headers 400), anyone can trigger
			// one with a casual request, and there is nothing for the server side to handle. The only server-side cause
			// is a ResponseWriter that does not support Hijack (500), an assembly error exposed by the first connection.
			slog.WarnContext(r.Context(), "stream: websocket upgrade failed",
				"path", r.URL.Path, "error", err)
			return
		}
		defer func() {
			if err := conn.Close(); err != nil {
				slog.DebugContext(r.Context(), "stream: websocket close", "path", r.URL.Path, "error", err)
			}
		}()
		if opt.ReadLimit > 0 {
			conn.SetReadLimit(opt.ReadLimit)
		}

		err = handler(r.Context(), conn)

		closeCode := websocket.CloseNormalClosure
		switch {
		case err == nil || isExpectedWSClose(err):
		case errors.Is(err, websocket.ErrReadLimit):
			// The client sent a frame over ReadLimit: its mistake, not a server failure, so no error log.
			closeCode = websocket.CloseMessageTooBig
			slog.WarnContext(r.Context(), "stream: websocket frame over the read limit", "path", r.URL.Path)
		default:
			closeCode = websocket.CloseInternalServerErr
			slog.ErrorContext(r.Context(), "stream: websocket handler failed",
				"path", r.URL.Path, "error", err)
		}
		deadline := time.Now().Add(opt.CloseTimeout)
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(closeCode, ""), deadline)
	}
}

// isExpectedWSClose reports whether the error is a normal client disconnect.
func isExpectedWSClose(err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	return websocket.IsCloseError(err,
		websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseNoStatusReceived,
		websocket.CloseAbnormalClosure, // the client dropping the connection (network loss/page closed) is normal for long-lived connections
	)
}
