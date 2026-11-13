package api

import (
	"fmt"
	"net/http"

	"github.com/go-keel/keel/internal/errs"

	khttp "github.com/go-kratos/kratos/v3/transport/http"
)

// HealthPath is the liveness check path every HTTP server has built in.
const HealthPath = "/health"

// registerHealth registers the liveness check after the server is built and before any business route: gorilla/mux
// matches in registration order, so placed first it is never shadowed by business routes or an SPA prefix route.
//
// It only means the process is alive and can respond; it does not probe dependencies such as the DB or Redis: load
// balancers probe every instance repeatedly, and probing dependencies would let one dependency blip pull all
// instances at once. The trade-off is in ADR keel-0009. It is a raw route that skips the middleware chain and needs
// no credential, even on servers with authentication enabled.
func registerHealth(srv *khttp.Server) {
	srv.HandleFunc(HealthPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("ok"))
		}
	})
}

var errHealthConflict = errs.Rules.New("api.health-route-conflict",
	fmt.Sprintf("%s is registered by Keel on every HTTP server, and one of your routes uses it too", HealthPath),
	"remove your own route; Keel answers liveness checks there")

// checkHealthConflict runs after contract endpoints are registered: if the user's proto also declares /health, the
// mux does not error but lets the framework's earlier route silently shadow it, and that endpoint is never called
// again. This turns it into a startup error. WalkRoute lists only routes that declare methods; the framework's route
// declares none and is not among them, so any match is a conflict. Conflicts with raw routes are caught when
// StreamRoute registers them.
func checkHealthConflict(srv *khttp.Server) error {
	conflict := false
	_ = srv.WalkRoute(func(info khttp.RouteInfo) error {
		if info.Path == HealthPath {
			conflict = true
		}
		return nil
	})
	if conflict {
		return errHealthConflict
	}
	return nil
}
