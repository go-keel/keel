package auth_test

import (
	"context"
	"testing"

	jwtv5 "github.com/golang-jwt/jwt/v5"

	"github.com/go-keel/keel/middleware/auth"
)

func TestFromContext(t *testing.T) {
	signedIn := auth.NewContext(context.Background(), jwtv5.MapClaims{"sub": "1"})

	if claims, ok := auth.FromContext(signedIn); !ok || claims["sub"] != "1" {
		t.Errorf("stored claims: got (%v, %v), want the claims and true", claims, ok)
	}
	if _, ok := auth.FromContext(context.Background()); ok {
		t.Error("no claims stored: ok = true, want false")
	}
	// nil claims over a parent's identity clear it: the caller must not count as signed in.
	if claims, ok := auth.FromContext(auth.NewContext(signedIn, nil)); ok || claims != nil {
		t.Errorf("nil claims over a signed-in parent: got (%v, %v), want (nil, false)", claims, ok)
	}
}
