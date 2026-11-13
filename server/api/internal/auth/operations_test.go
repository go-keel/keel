package auth_test

import (
	"errors"
	"testing"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/pkg/jwt"
	"github.com/go-keel/keel/server/api/internal/auth"
)

// Security-critical: Build takes a raw conf message, not guaranteed to have passed kconfig's load-time validation. With
// authentication on, HS256 and an empty secret it must error; otherwise the verifier verifies with an empty secret, which is no verification at all.
func TestBuild_HS256EmptySecretRejected(t *testing.T) {
	_, err := auth.Build(&conf.Server_Auth{Enabled: true, Algorithm: "HS256"})
	if !errors.Is(err, jwt.ErrEmptySecret) {
		t.Fatalf("want jwt.ErrEmptySecret, got %v", err)
	}
}
