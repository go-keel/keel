package sentinel

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	kerrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/go-kratos/kratos/v3/middleware"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/go-keel/keel/internal/errs"
	"github.com/go-keel/keel/middleware/locale"
)

// HandlerFunc converts a matched sentinel error into a transport-level error.
type HandlerFunc func(ctx context.Context, err error) error

type rule struct {
	sentinel error
	handler  HandlerFunc
}

// Group maps business sentinel errors to Kratos errors.
type Group struct {
	rules       []rule
	defaultFunc HandlerFunc
	// err accumulates registration errors (a nil sentinel or handler), returned by Err and joined into the chain's
	// error by mw.WithSentinel, so a mapping that would never run fails startup instead of being dropped.
	err error
}

// NewGroup creates a sentinel group with InternalWithLog as the default handler.
func NewGroup() *Group {
	return &Group{defaultFunc: InternalWithLog}
}

// Err returns the accumulated registration errors, if any. mw.WithSentinel records them on the chain, so the server
// constructor returns them at startup.
func (g *Group) Err() error { return g.err }

func (g *Group) invalid(what string) {
	g.err = errors.Join(g.err, errs.Rules.New("sentinel.rule-invalid", what, "pass a non-nil sentinel error, handler, message and build function, or drop the call"))
}

// On registers a handler for errors matching sentinel via errors.Is. A nil sentinel or handler records an error
// returned by Err.
func (g *Group) On(sentinel error, handler HandlerFunc) *Group {
	if sentinel == nil || handler == nil {
		g.invalid("sentinel.Group.On was called with a nil sentinel or a nil handler")
		return g
	}
	g.rules = append(g.rules, rule{sentinel: sentinel, handler: handler})
	return g
}

// OnMessage registers the common localized mapping: errors matching sentinel become build(<msg in the request's
// language>), where build is a typed error constructor generated from an error_reason enum (v1.ErrorNoteNotFound).
// Translation falls back like locale.Localize. Write msg as a literal so goi18n extract finds it. A nil sentinel, msg
// or build records an error returned by Err.
func (g *Group) OnMessage(sentinel error, msg *i18n.Message, build func(format string, args ...any) *kerrors.Error) *Group {
	if sentinel == nil || msg == nil || build == nil {
		g.invalid("sentinel.Group.OnMessage was called with a nil sentinel, a nil message or a nil build function")
		return g
	}
	return g.On(sentinel, func(ctx context.Context, _ error) error {
		return build("%s", locale.Localize(ctx, msg))
	})
}

// Default overrides the fallback handler for unmatched errors. A nil handler records an error returned by Err.
func (g *Group) Default(handler HandlerFunc) *Group {
	if handler == nil {
		g.invalid("sentinel.Group.Default was called with a nil handler")
		return g
	}
	g.defaultFunc = handler
	return g
}

// Middleware returns the kratos middleware for this group.
func (g *Group) Middleware() middleware.Middleware {
	return func(next middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			resp, err := next(ctx, req)
			if err == nil {
				return resp, nil
			}
			return resp, g.translate(ctx, err)
		}
	}
}

func (g *Group) translate(ctx context.Context, err error) error {
	for _, r := range g.rules {
		if errors.Is(err, r.sentinel) {
			return r.handler(ctx, err)
		}
	}
	// Pass through Kratos microservice errors (carry Details).
	st, isStatus := status.FromError(err)
	if isStatus && len(st.Details()) > 0 {
		return st.Err()
	}
	// Common context errors, also as gRPC returns them (a stream ended by its client, such as a health Watch when the
	// client closes, comes back as status Canceled, not as context.Canceled).
	// Note: kratos errors.Code is an HTTP status code; do not use gRPC codes (1/4 are invalid).
	switch {
	case errors.Is(err, context.Canceled) || isStatus && st.Code() == codes.Canceled:
		// 499: client closed request (nginx convention)
		return kerrors.New(499, "CANCELED", "request canceled")
	case errors.Is(err, context.DeadlineExceeded) || isStatus && st.Code() == codes.DeadlineExceeded:
		return kerrors.New(504, "DEADLINE_EXCEEDED", "request deadline exceeded")
	}
	return g.defaultFunc(ctx, err)
}

// InternalWithLog logs the error and returns a generic internal server error.
func InternalWithLog(ctx context.Context, err error) error {
	slog.ErrorContext(ctx, "sentinel: unhandled error", "error", err)
	return kerrors.New(kerrors.UnknownCode, "INTERNAL", "internal error")
}

// Convenience HandlerFunc constructors.
//
// The outgoing Message of these takes the sentinel's own text, not the full text of the error received: `On`
// matches with `errors.Is`, so `fmt.Errorf("query todos: %w", biz.ErrTodoNotFound)` matches too, and its `Error()`
// carries the context the caller added. That context is for developers, but Message is shown verbatim to end users
// (a frontend may show it directly and keep no second copy of the wording). So the sentinel's
// own sentence is the sentence users read, and should be written to that standard; the details needed for
// debugging are wrapped in `%w` as usual and only reach the logs.

// NotFound maps to a 404 with the given reason.
func NotFound(reason string) HandlerFunc {
	return func(_ context.Context, err error) error {
		return kerrors.NotFound(reason, rootMessage(err))
	}
}

// Conflict maps to a 409 with the given reason.
func Conflict(reason string) HandlerFunc {
	return func(_ context.Context, err error) error {
		return kerrors.Conflict(reason, rootMessage(err))
	}
}

// BadRequest maps to a 400 with the given reason.
func BadRequest(reason string) HandlerFunc {
	return func(_ context.Context, err error) error {
		return kerrors.BadRequest(reason, rootMessage(err))
	}
}

// PreconditionFailed maps to a 412 with the given reason: the request is valid
// but the resource's current state rules it out (gRPC FAILED_PRECONDITION).
func PreconditionFailed(reason string) HandlerFunc {
	return func(_ context.Context, err error) error {
		return kerrors.New(http.StatusPreconditionFailed, reason, rootMessage(err))
	}
}

// rootMessage takes the text of the deepest layer of the error chain: by convention a sentinel is a stdlib
// `errors.New` value in the biz layer (see docs/middleware.md "Sentinel"),
// at the very bottom of the chain, so unwrapping to the end yields its own sentence.
//
// It follows only the Unwrap() error chain. A multi-error from errors.Join returns nil from Unwrap, so the
// merged full text is taken as is; deal with it when multi-sentinels are actually used, and then the right move is
// to pass the message explicitly rather than guess.
func rootMessage(err error) string {
	for {
		unwrapped := errors.Unwrap(err)
		if unwrapped == nil {
			return err.Error()
		}
		err = unwrapped
	}
}
