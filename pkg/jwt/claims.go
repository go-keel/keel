package jwt

import (
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

// NewClaims builds MapClaims with correctly typed standard fields, avoiding a pitfall of writing them by hand:
// jwtv5.MapClaims requires exp/iat/nbf to be float64 (unix seconds); passing int64
// (such as time.Time.Unix()) fails Sign/Verify with "invalid type for claim".
//
// subject goes into "sub", the expiry after ttl into "exp", and the issue time into "iat".
// Add custom fields by assigning on the returned value:
//
//	c := jwt.NewClaims("user-1", time.Hour)
//	c["role"] = "admin"
//	token, _ := signer.Sign(c)
func NewClaims(subject string, ttl time.Duration) jwtv5.MapClaims {
	now := time.Now()
	return jwtv5.MapClaims{
		"sub": subject,
		"iat": float64(now.Unix()),
		"exp": float64(now.Add(ttl).Unix()),
	}
}
