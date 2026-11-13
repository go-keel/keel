package pwd_test

import (
	"testing"

	"github.com/go-keel/keel/pkg/pwd"
)

func TestHashVerify_RoundTrip(t *testing.T) {
	hash, err := pwd.HashPassword("s3cr3t-pass")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash == "s3cr3t-pass" {
		t.Fatal("hash must not equal plaintext")
	}
	ok, err := pwd.VerifyPassword("s3cr3t-pass", hash)
	if err != nil || !ok {
		t.Errorf("correct password should verify: ok=%v err=%v", ok, err)
	}
}

func TestVerify_WrongPassword(t *testing.T) {
	hash, _ := pwd.HashPassword("right")
	ok, err := pwd.VerifyPassword("wrong", hash)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ok {
		t.Error("wrong password must not verify")
	}
}

func TestHash_Salted(t *testing.T) {
	// Argon2id uses a random salt: hashing the same password twice must differ.
	h1, _ := pwd.HashPassword("same")
	h2, _ := pwd.HashPassword("same")
	if h1 == h2 {
		t.Error("hashes of same password must differ (random salt)")
	}
}
