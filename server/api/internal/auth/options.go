// Package auth holds the implementation details of authentication: building the verifier from Server_Auth config,
// and constructing the contract endpoint middleware and the raw route filters. All three share the same token
// extraction and verification logic, so the two kinds of endpoint on one server behave identically case by case;
// that is exactly why they must be built together in one Build (see build.go).
//
// It lives under server/api/internal/: only server/api needs these constructors (the server builds authentication,
// see build.go), and Go's internal visibility rule naturally locks it inside this subtree.
// Externally only middleware/auth exposes FromContext/NewContext/Client
// and three error values; this package is not a public API.
package auth

// Option configures the construction of the auth middleware and filters.
type Option func(*options)

type options struct {
	cookieName string
}

// WithCookie makes verification fall back to reading the token from the cookie of this name when the Authorization
// header is missing. The header wins over the cookie: a caller that explicitly sends the header has clearer intent and
// must not be overridden by a cookie the browser sends along. Browsers attach cookies to cross-site requests
// automatically, so the issuer must set SameSite (Strict/Lax) to block CSRF; this package only extracts the token
// and does not issue it.
func WithCookie(name string) Option {
	return func(o *options) { o.cookieName = name }
}

func newOptions(opts ...Option) *options {
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}
	return o
}
