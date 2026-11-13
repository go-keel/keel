package jwt

import (
	"crypto/rsa"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

// Verifier is the JWT verifier interface.
type Verifier interface {
	Verify(token string) (jwtv5.MapClaims, error)
}

// =============================================================================
// RS256 verifier (asymmetric)
// =============================================================================

// NewRS256Verifier creates an RS256 JWT verifier.
//
// It verifies signatures with an RSA public key.
// Suited to communication between microservices; the public key can be safely distributed to every verifying service.
//
// Parameters:
//   - key: the RSA public key; LoadRSAPublicKey() can load it
//
// Example:
//
//	publicKey, _ := jwt.LoadRSAPublicKey("path/to/public.pem")
//	verifier := jwt.NewRS256Verifier(publicKey)
//	claims, err := verifier.Verify(token)
//	if err != nil {
//	    // invalid token
//	}
func NewRS256Verifier(key *rsa.PublicKey) Verifier {
	return &rs256Verifier{
		keyFun: func(*jwtv5.Token) (any, error) {
			return key, nil
		},
	}
}

type rs256Verifier struct {
	keyFun jwtv5.Keyfunc
}

func (v *rs256Verifier) Verify(token string) (jwtv5.MapClaims, error) {
	claims := jwtv5.MapClaims{}
	_, err := jwtv5.ParseWithClaims(token, claims, v.keyFun,
		jwtv5.WithValidMethods([]string{jwtv5.SigningMethodRS256.Alg()}),
		jwtv5.WithExpirationRequired(),
	)
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// =============================================================================
// HS256 verifier (symmetric)
// =============================================================================

// NewHS256Verifier creates an HS256 JWT verifier.
//
// It verifies signatures with an HMAC-SHA256 symmetric key.
// Suited to simple scenarios or monoliths.
//
// Parameters:
//   - secret: the key bytes, which must equal the key used for signing; empty returns ErrEmptySecret
//
// Example:
//
//	secret := []byte("your-256-bit-secret-key-here-!!!")
//	verifier, err := jwt.NewHS256Verifier(secret)
//	if err != nil {
//	    // empty secret
//	}
//	claims, err := verifier.Verify(token)
//	if err != nil {
//	    // invalid token
//	}
func NewHS256Verifier(secret []byte) (Verifier, error) {
	if len(secret) == 0 {
		return nil, ErrEmptySecret
	}
	return &hs256Verifier{secret: secret}, nil
}

type hs256Verifier struct {
	secret []byte
}

func (v *hs256Verifier) Verify(token string) (jwtv5.MapClaims, error) {
	claims := jwtv5.MapClaims{}
	_, err := jwtv5.ParseWithClaims(token, claims,
		func(*jwtv5.Token) (any, error) {
			return v.secret, nil
		},
		jwtv5.WithValidMethods([]string{jwtv5.SigningMethodHS256.Alg()}),
		jwtv5.WithExpirationRequired(),
	)
	if err != nil {
		return nil, err
	}
	return claims, nil
}
