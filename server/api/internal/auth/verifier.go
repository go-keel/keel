package auth

import (
	"fmt"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/internal/errs"
	"github.com/go-keel/keel/pkg/jwt"
)

// verifierFromConfig constructs a jwt.Verifier from a Server_Auth config.
func verifierFromConfig(cfg *conf.Server_Auth) (jwt.Verifier, error) {
	switch cfg.GetAlgorithm() {
	case "RS256", "":
		pub, err := jwt.LoadRSAPublicKey(cfg.GetJwtPublicKeyFile())
		if err != nil {
			return nil, fmt.Errorf("auth.jwt_public_key_file: %w", err)
		}
		return jwt.NewRS256Verifier(pub), nil
	case "HS256":
		// An empty secret is rejected by jwt.NewHS256Verifier itself: this takes a raw conf message, which is not
		// guaranteed to have passed kconfig's load-time validation.
		return jwt.NewHS256Verifier([]byte(cfg.GetJwtSecret()))
	default:
		return nil, errs.Rules.New("api.auth-algorithm", fmt.Sprintf("auth.algorithm is %q, which Keel does not support", cfg.GetAlgorithm()), "set auth.algorithm to RS256 (the default) or HS256")
	}
}
