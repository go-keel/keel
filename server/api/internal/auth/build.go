package auth

import (
	"net/http"

	"github.com/go-kratos/kratos/v3/middleware"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/internal/errs"
)

// Built is the one-off authentication components built from one server's own Server_Auth config: the contract
// endpoint middleware and the two raw route filters share the same verifier and options, so one config reads and
// errors identically on all three paths (unary/streaming, login-required raw routes, anonymous raw routes).
type Built struct {
	Middleware   middleware.Middleware
	RequireLogin func(http.Handler) http.Handler
	Anonymous    func(http.Handler) http.Handler
}

// Build builds authentication once from cfg. It returns (nil, nil) when cfg is nil or disabled; callers then keep the
// auth slot reserved by mw.Default as a placeholder and the two raw route entries without verification.
func Build(cfg *conf.Server_Auth) (*Built, error) {
	if cfg == nil || !cfg.GetEnabled() {
		return nil, nil
	}
	verifier, err := verifierFromConfig(cfg)
	if err != nil {
		return nil, errs.While("auth: build JWT verifier", err)
	}
	var opts []Option
	if name := cfg.GetCookieName(); name != "" {
		opts = append(opts, WithCookie(name))
	}
	return &Built{
		Middleware:   ServerMiddleware(verifier, opts...),
		RequireLogin: Filter(verifier, true, opts...),
		Anonymous:    Filter(verifier, false, opts...),
	}, nil
}
