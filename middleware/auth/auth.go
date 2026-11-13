// Package auth exposes only reading the authentication result (FromContext), writing it (NewContext), token
// injection for service-to-service calls (Client) and three error values. The "how to check" details (building
// the verifier, the middleware and the raw route filter) live in internal/auth: authentication is built by the
// server (api.NewHTTPServer / NewGRPCServer) from its own Server_Auth config, and there is no separately
// assemblable building block exposed.
package auth

import (
	"context"

	"github.com/go-kratos/kratos/v3/errors"
	"github.com/go-kratos/kratos/v3/middleware"
	"github.com/go-kratos/kratos/v3/transport"
	jwtv5 "github.com/golang-jwt/jwt/v5"
)

// All three share the same Code/Reason, and kratos's errors.Is compares only those two, so they test equal to each
// other. This is by design: users only need to know "authentication failed", not which kind; Message is for humans.
//
// The Message of the first two is shown verbatim to end users (a frontend may show it directly),
// so it does not name an implementation choice like JWT: swap the auth implementation and "JWT missing" becomes a lie.
// (`invalid token` stays: token here is the generic word for "credential", not tied to any implementation.)
// ErrWrongContext is the opposite: it appears only when a user mounts the middleware in the wrong place, its reader
// is a developer, and it keeps technical wording.
var (
	ErrInvalidToken    = errors.Unauthorized("UNAUTHORIZED", "invalid token")
	ErrMissingJwtToken = errors.Unauthorized("UNAUTHORIZED", "not signed in")
	ErrWrongContext    = errors.Unauthorized("UNAUTHORIZED", "wrong context for middleware")
)

// claimsCtxKey is the context key for storing JWT claims.
type claimsCtxKey struct{}

// NewContext stores JWT claims in the context.
func NewContext(ctx context.Context, claims jwtv5.Claims) context.Context {
	return context.WithValue(ctx, claimsCtxKey{}, claims)
}

const (
	// bearerWord the bearer key word for authorization
	bearerWord string = "Bearer"

	// authorizationKey holds the key used to store the Jwt Token in the request tokenHeader.
	authorizationKey string = "Authorization"
)

// FromContext extracts the Jwt claims from the context.
// The second return is false if no claims are present or they are not jwtv5.MapClaims.
func FromContext(ctx context.Context) (jwtv5.MapClaims, bool) {
	claims, ok := ctx.Value(claimsCtxKey{}).(jwtv5.MapClaims)
	return claims, ok
}

// Client creates a middleware that adds the Jwt token to the request header.
// The middleware retrieves the token using the provided token function and sets it in the request header.
// The token function should return a valid Jwt token string.
func Client(token func() string) middleware.Middleware {
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, request any) (response any, err error) {
			if clientContext, ok := transport.FromClientContext(ctx); ok {
				clientContext.RequestHeader().Set(authorizationKey, bearerWord+" "+token())
				return handler(ctx, request)
			}
			return nil, ErrWrongContext
		}
	}
}
