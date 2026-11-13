package auth

import (
	"net/http"

	khttp "github.com/go-kratos/kratos/v3/transport/http"

	"github.com/go-keel/keel/pkg/jwt"

	keelauth "github.com/go-keel/keel/middleware/auth"
)

// Filter is the http.Handler wrapper for raw routes (bypassing the middleware chain). With required=true a missing or
// invalid credential is always rejected (StreamContext.RequireLogin); with required=false both pass, but a valid
// credential still writes the identity (StreamContext.Anonymous), consistent with the contract endpoints' anonymous
// allow-list.
//
// Rejection reuses khttp.DefaultErrorEncoder (server/api does not customise ErrorEncoder, and contract endpoints'
// authentication failures are written by it too), producing the same structured error and distinguishing the same two
// causes, ErrMissingJwtToken / ErrInvalidToken, matching the contract endpoints' failure format.
func Filter(verifier jwt.Verifier, required bool, opts ...Option) func(http.Handler) http.Handler {
	o := newOptions(opts...)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, err := verifyFromRequest(r, verifier, o.cookieName)
			if err != nil {
				if !required {
					next.ServeHTTP(w, r)
					return
				}
				khttp.DefaultErrorEncoder(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(keelauth.NewContext(r.Context(), claims)))
		})
	}
}
