package jwt

import (
	"crypto/rsa"
	"fmt"
	"os"

	jwtv5 "github.com/golang-jwt/jwt/v5"

	"github.com/go-keel/keel/internal/errs"
)

// LoadRSAPrivateKey reads an RSA private key from a PEM file (PKCS#1 or PKCS#8). Parsing is golang-jwt's; this adds
// rule errors that name the file.
func LoadRSAPrivateKey(path string) (*rsa.PrivateKey, error) {
	b, err := readKeyFile(path)
	if err != nil {
		return nil, err
	}
	k, err := jwtv5.ParseRSAPrivateKeyFromPEM(b)
	if err != nil {
		return nil, errs.Rules.Wrap(err, "jwt.private-key-invalid", fmt.Sprintf("%q is not a PEM-encoded RSA private key", path), "point to the private key PEM (PKCS#1 \"RSA PRIVATE KEY\" or PKCS#8 \"PRIVATE KEY\"), such as one made by openssl genrsa")
	}
	return k, nil
}

// LoadRSAPublicKey reads an RSA public key from a PEM file (PKIX "PUBLIC KEY", as openssl rsa -pubout writes, PKCS#1
// "RSA PUBLIC KEY", or an X.509 certificate). Parsing is golang-jwt's; this adds rule errors that name the file.
func LoadRSAPublicKey(path string) (*rsa.PublicKey, error) {
	b, err := readKeyFile(path)
	if err != nil {
		return nil, err
	}
	k, err := jwtv5.ParseRSAPublicKeyFromPEM(b)
	if err != nil {
		return nil, errs.Rules.Wrap(err, "jwt.public-key-invalid", fmt.Sprintf("%q is not a PEM-encoded RSA public key", path), "point to the public key PEM written by openssl rsa -in private.pem -pubout, or to an X.509 certificate")
	}
	return k, nil
}

func readKeyFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errs.Rules.Wrap(err, "jwt.key-file-unreadable", fmt.Sprintf("cannot read the key file %q", path), "check the path in the config (relative paths start from the working directory) and the file's permissions")
	}
	return b, nil
}
