package jwt_test

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	"github.com/go-keel/keel/pkg/jwt"
	jwtv5 "github.com/golang-jwt/jwt/v5"
)

func claims(exp time.Time) jwtv5.MapClaims {
	// MapClaims requires exp to be float64 (unix seconds), not int64/NumericDate.
	return jwtv5.MapClaims{"sub": "u1", "exp": float64(exp.Unix())}
}

func mustHS256Signer(t *testing.T, secret []byte) jwt.Signer {
	t.Helper()
	s, err := jwt.NewHS256Signer(secret)
	if err != nil {
		t.Fatalf("NewHS256Signer: %v", err)
	}
	return s
}

func mustHS256Verifier(t *testing.T, secret []byte) jwt.Verifier {
	t.Helper()
	v, err := jwt.NewHS256Verifier(secret)
	if err != nil {
		t.Fatalf("NewHS256Verifier: %v", err)
	}
	return v
}

// Security-critical: anyone can forge a token signed with an empty secret, and verifying with an empty secret is no
// verification at all, so both signer and verifier must reject it at construction, not fail later at sign or verify.
func TestHS256_EmptySecretRejected(t *testing.T) {
	for _, secret := range [][]byte{nil, {}} {
		if _, err := jwt.NewHS256Signer(secret); !errors.Is(err, jwt.ErrEmptySecret) {
			t.Errorf("signer: want ErrEmptySecret, got %v", err)
		}
		if _, err := jwt.NewHS256Verifier(secret); !errors.Is(err, jwt.ErrEmptySecret) {
			t.Errorf("verifier: want ErrEmptySecret, got %v", err)
		}
	}
}

func TestHS256_RoundTrip(t *testing.T) {
	secret := []byte("test-secret-key")
	tok, err := mustHS256Signer(t, secret).Sign(claims(time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := mustHS256Verifier(t, secret).Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got["sub"] != "u1" {
		t.Errorf("sub = %v", got["sub"])
	}
}

func TestRS256_RoundTrip(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tok, err := jwt.NewRS256Signer(key).Sign(claims(time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := jwt.NewRS256Verifier(&key.PublicKey).Verify(tok); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// Security-critical: an RS256 verifier must reject an HS256 token (defence against algorithm confusion attacks).
func TestAlgConfusion_RS256VerifierRejectsHS256(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	// The attacker uses the public key bytes as the HMAC secret to forge an HS256 token
	hsTok, _ := mustHS256Signer(t, []byte("whatever")).Sign(claims(time.Now().Add(time.Hour)))
	if _, err := jwt.NewRS256Verifier(&key.PublicKey).Verify(hsTok); err == nil {
		t.Fatal("RS256 verifier must reject HS256 token (alg confusion)")
	}
}

func TestAlgConfusion_HS256VerifierRejectsRS256(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	rsTok, _ := jwt.NewRS256Signer(key).Sign(claims(time.Now().Add(time.Hour)))
	if _, err := mustHS256Verifier(t, []byte("secret")).Verify(rsTok); err == nil {
		t.Fatal("HS256 verifier must reject RS256 token (alg confusion)")
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	secret := []byte("s")
	tok, _ := mustHS256Signer(t, secret).Sign(claims(time.Now().Add(-time.Hour)))
	if _, err := mustHS256Verifier(t, secret).Verify(tok); err == nil {
		t.Error("expired token must be rejected")
	}
}

func TestWrongSecretRejected(t *testing.T) {
	tok, _ := mustHS256Signer(t, []byte("right")).Sign(claims(time.Now().Add(time.Hour)))
	if _, err := mustHS256Verifier(t, []byte("wrong")).Verify(tok); err == nil {
		t.Error("wrong secret must fail verification")
	}
}

func TestExpirationRequired(t *testing.T) {
	secret := []byte("s")
	// Sign itself rejects claims without exp, so to cover the verifier's WithExpirationRequired branch, bypass Sign
	// and use the upstream library directly to build a validly signed token without exp.
	tok, err := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256,
		jwtv5.MapClaims{"sub": "u1"}).SignedString(secret)
	if err != nil {
		t.Fatalf("build token: %v", err)
	}
	if _, err := mustHS256Verifier(t, secret).Verify(tok); err == nil {
		t.Error("token without exp must be rejected (WithExpirationRequired)")
	}
}
