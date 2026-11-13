package auth

import (
	"net/http"
	"strings"

	"github.com/go-kratos/kratos/v3/transport"
	khttp "github.com/go-kratos/kratos/v3/transport/http"
	jwtv5 "github.com/golang-jwt/jwt/v5"

	"github.com/go-keel/keel/pkg/jwt"

	keelauth "github.com/go-keel/keel/middleware/auth"
)

const (
	bearerWord       = "Bearer"
	authorizationKey = "Authorization"
)

// extractToken gets the JWT: Authorization: Bearer first, then (when cookieName is set) the cookie of that name.
// The unary middleware and the raw route filters share tokenFromRequest, so extraction semantics stay identical.
func extractToken(tr transport.Transporter, cookieName string) (string, error) {
	if htr, ok := tr.(khttp.Transporter); ok {
		return tokenFromRequest(htr.Request(), cookieName)
	}
	// Cookies exist only on the HTTP transport; gRPC and other transports have only the Authorization header.
	if token, ok := bearerFromHeader(tr.RequestHeader().Get(authorizationKey)); ok {
		return token, nil
	}
	return "", keelauth.ErrMissingJwtToken
}

// tokenFromRequest is the single implementation of "header first, cookie fallback".
func tokenFromRequest(r *http.Request, cookieName string) (string, error) {
	if token, ok := bearerFromHeader(r.Header.Get(authorizationKey)); ok {
		return token, nil
	}
	if cookieName == "" {
		return "", keelauth.ErrMissingJwtToken
	}
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return "", keelauth.ErrMissingJwtToken
	}
	return c.Value, nil
}

// bearerFromHeader extracts the token from a header value of the form "Bearer <token>".
func bearerFromHeader(v string) (string, bool) {
	scheme, token, ok := strings.Cut(v, " ")
	if !ok || !strings.EqualFold(scheme, bearerWord) {
		return "", false
	}
	return token, true
}

// verifyFromTransport is for contract endpoints (unary/stream establishment): it gets the token and verifies it. The
// returned error is normalised to one of ErrMissingJwtToken / ErrInvalidToken; the specific verification failure
// (bad signature, expired, wrong algorithm...) does not leak.
func verifyFromTransport(tr transport.Transporter, verifier jwt.Verifier, cookieName string) (jwtv5.MapClaims, error) {
	token, err := extractToken(tr, cookieName)
	if err != nil {
		return nil, err
	}
	claims, err := verifier.Verify(token)
	if err != nil {
		return nil, keelauth.ErrInvalidToken
	}
	return claims, nil
}

// verifyFromRequest is for raw routes: same semantics as verifyFromTransport, but it reads from *http.Request rather
// than transport.Transporter, because raw routes are not on the middleware chain and have no transport.Transporter.
func verifyFromRequest(r *http.Request, verifier jwt.Verifier, cookieName string) (jwtv5.MapClaims, error) {
	token, err := tokenFromRequest(r, cookieName)
	if err != nil {
		return nil, err
	}
	claims, err := verifier.Verify(token)
	if err != nil {
		return nil, keelauth.ErrInvalidToken
	}
	return claims, nil
}
