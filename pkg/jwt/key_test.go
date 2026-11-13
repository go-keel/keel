package jwt_test

import (
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-keel/keel/pkg/jwt"
)

// A PEM wrapper with corrupt content (of every accepted block type) and a missing file fail with rule errors naming
// the file.
func TestLoadRSAKey_Errors(t *testing.T) {
	load := map[string]func(string) error{
		"RSA PRIVATE KEY": func(p string) error { _, err := jwt.LoadRSAPrivateKey(p); return err },
		"PRIVATE KEY":     func(p string) error { _, err := jwt.LoadRSAPrivateKey(p); return err },
		"RSA PUBLIC KEY":  func(p string) error { _, err := jwt.LoadRSAPublicKey(p); return err },
		"PUBLIC KEY":      func(p string) error { _, err := jwt.LoadRSAPublicKey(p); return err },
	}
	for typ, fn := range load {
		path := filepath.Join(t.TempDir(), "key.pem")
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: []byte("garbage")}), 0o600); err != nil {
			t.Fatal(err)
		}
		want := "jwt.public-key-invalid"
		if typ == "RSA PRIVATE KEY" || typ == "PRIVATE KEY" {
			want = "jwt.private-key-invalid"
		}
		if err := fn(path); err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: got %v, want %s naming the file", typ, err, want)
		}
	}
	missing := filepath.Join(t.TempDir(), "missing.pem")
	if _, err := jwt.LoadRSAPublicKey(missing); err == nil || !strings.Contains(err.Error(), "jwt.key-file-unreadable") || !strings.Contains(err.Error(), missing) {
		t.Errorf("missing file: got %v, want jwt.key-file-unreadable naming the file", err)
	}
}
