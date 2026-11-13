package jwt

import (
	"crypto/rsa"
	"errors"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

var (
	ErrClaimsMissingExpirationTime = errors.New("claims missing expiration time")

	// ErrEmptySecret means the HS256 secret is empty. Anyone can forge a token signed with an empty secret, and
	// verifying with an empty secret is no verification at all, so signer and verifier reject it at construction.
	ErrEmptySecret = errors.New("jwt: HS256 secret must not be empty")
)

type Signer interface {
	Sign(claims jwtv5.MapClaims) (string, error)
}

// NewRS256Signer creates an RS256 JWT signer.
//
// It signs with an RSA private key.
// Suited to communication between microservices; the public key can be safely distributed to every verifying service.
func NewRS256Signer(key *rsa.PrivateKey) Signer {
	return &rs256Signer{key: key}
}

// NewHS256Signer creates an HS256 JWT signer.
//
// It signs with an HMAC-SHA256 symmetric key.
// Suited to simple scenarios or monoliths. An empty secret returns ErrEmptySecret.
func NewHS256Signer(secret []byte) (Signer, error) {
	if len(secret) == 0 {
		return nil, ErrEmptySecret
	}
	return &hs256Signer{secret: secret}, nil
}

type rs256Signer struct {
	key *rsa.PrivateKey
}

func (j *rs256Signer) Sign(claims jwtv5.MapClaims) (string, error) {
	if err := validateClaimsExpiration(claims); err != nil {
		return "", err
	}
	return jwtv5.NewWithClaims(jwtv5.SigningMethodRS256, claims).SignedString(j.key)
}

type hs256Signer struct {
	secret []byte
}

func (j *hs256Signer) Sign(claims jwtv5.MapClaims) (string, error) {
	if err := validateClaimsExpiration(claims); err != nil {
		return "", err
	}
	return jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, claims).SignedString(j.secret)
}

func validateClaimsExpiration(claims jwtv5.MapClaims) error {
	expirationTime, err := claims.GetExpirationTime()
	if err != nil {
		return err
	}
	if expirationTime == nil || expirationTime.IsZero() {
		return ErrClaimsMissingExpirationTime
	}
	return nil
}
