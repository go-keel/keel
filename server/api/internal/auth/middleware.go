package auth

import (
	"context"

	"github.com/go-kratos/kratos/v3/middleware"
	"github.com/go-kratos/kratos/v3/transport"

	"github.com/go-keel/keel/pkg/jwt"

	keelauth "github.com/go-keel/keel/middleware/auth"
)

// ServerMiddleware creates the JWT authentication middleware for contract endpoints: it goes into mw.SlotAuth,
// and unary requests and gRPC streaming (at stream establishment) share the same logic.
func ServerMiddleware(verifier jwt.Verifier, opts ...Option) middleware.Middleware {
	o := newOptions(opts...)
	wl := getGlobalOperationWhiteList()

	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, request any) (response any, err error) {
			serverContext, ok := transport.FromServerContext(ctx)
			if !ok {
				return nil, keelauth.ErrWrongContext
			}

			claims, verr := verifyFromTransport(serverContext, verifier, o.cookieName)

			// Endpoints that do not allow anonymous access return an authentication failure. verr is already one of
			// ErrMissingJwtToken or ErrInvalidToken (verifyFromTransport decides directly by the failing step,
			// extracting the token or verifying the signature, so pass it through as is); do not infer it here with
			// errors.Is(verr, ErrMissingJwtToken): the two error values share Code/Reason
			// (both 401/UNAUTHORIZED, only Message differs), and kratos's
			// (*Error).Is compares only Code+Reason, so it would mistake one for the other.
			if verr != nil && !wl.Contains(serverContext.Operation()) {
				return nil, verr
			}

			// Allow-listed endpoints allow anonymous access, but a valid token still puts the claims in the context.
			if claims != nil {
				ctx = keelauth.NewContext(ctx, claims)
			}
			return handler(ctx, request)
		}
	}
}
