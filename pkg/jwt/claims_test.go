package jwt_test

import (
	"testing"
	"time"

	"github.com/go-keel/keel/pkg/jwt"
)

func TestNewClaims(t *testing.T) {
	c := jwt.NewClaims("user-1", time.Hour)
	if c["sub"] != "user-1" {
		t.Errorf("sub = %v", c["sub"])
	}
	// exp/iat must be float64 (otherwise Sign reports invalid type)
	if _, ok := c["exp"].(float64); !ok {
		t.Errorf("exp must be float64, got %T", c["exp"])
	}
	// End to end: claims built by NewClaims can be signed and verified
	secret := []byte("s")
	tok, err := mustHS256Signer(t, secret).Sign(c)
	if err != nil {
		t.Fatalf("sign NewClaims output: %v", err)
	}
	if _, err := mustHS256Verifier(t, secret).Verify(tok); err != nil {
		t.Errorf("verify: %v", err)
	}
}
