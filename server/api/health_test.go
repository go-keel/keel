package api_test

// Behaviour anchors of the liveness check: built into every HTTP server, needs no credential even with
// authentication on, sits before prefix routes such as an SPA, and a duplicate registration by the user fails
// startup. Verified by starting real servers via NewHTTPServer.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	khttp "github.com/go-kratos/kratos/v3/transport/http"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/mw"
	stream "github.com/go-keel/keel/server/api"
)

func startHealthServer(t *testing.T, streamRegister ...stream.StreamRegister) string {
	t.Helper()
	srvT, err := stream.NewHTTPServer(&conf.Server_HTTP{
		Addr: "127.0.0.1:0",
		Auth: rawAuthConfig(),
	}, func(s *khttp.Server) {}, mw.Default(nil), streamRegister...)
	if err != nil {
		t.Fatalf("NewHTTPServer: %v", err)
	}
	srv := srvT.(*khttp.Server)
	ep, err := srv.Endpoint()
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	go func() { _ = srv.Start(context.Background()) }()
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	return "http://" + ep.Host
}

// On a server with authentication, liveness can be probed without a credential; GET returns ok, HEAD has no body, other methods 405.
func TestHealth_AnonymousOnAuthServer(t *testing.T) {
	base := startHealthServer(t)

	req, _ := http.NewRequest(http.MethodGet, base+stream.HealthPath, nil)
	if code, body := getBody(t, req); code != http.StatusOK || body != "ok" {
		t.Errorf("GET: status=%d body=%q, want 200/ok", code, body)
	}

	req, _ = http.NewRequest(http.MethodHead, base+stream.HealthPath, nil)
	if code, body := getBody(t, req); code != http.StatusOK || body != "" {
		t.Errorf("HEAD: status=%d body=%q, want 200 with empty body", code, body)
	}

	req, _ = http.NewRequest(http.MethodPost, base+stream.HealthPath, nil)
	if code, _ := getBody(t, req); code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status=%d, want 405", code)
	}
}

// A prefix route mounted at the root, such as an SPA, does not swallow /health.
func TestHealth_WinsOverPrefixRoute(t *testing.T) {
	base := startHealthServer(t, func(sc *stream.StreamContext) {
		sc.Anonymous().HandlePrefix("/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("spa"))
		}))
	})

	req, _ := http.NewRequest(http.MethodGet, base+stream.HealthPath, nil)
	if code, body := getBody(t, req); code != http.StatusOK || body != "ok" {
		t.Errorf("status=%d body=%q, want 200/ok from the health route, not the prefix route", code, body)
	}
}

// The user registering /health again: the mux does not error and would silently shadow it, so this must become a startup error.
func TestHealth_ConflictFailsStartup(t *testing.T) {
	_, err := stream.NewHTTPServer(&conf.Server_HTTP{Addr: "127.0.0.1:0"},
		func(s *khttp.Server) {}, mw.Default(nil),
		func(sc *stream.StreamContext) {
			sc.Anonymous().HandleFunc(stream.HealthPath, func(http.ResponseWriter, *http.Request) {})
		})
	if err == nil || !strings.Contains(err.Error(), stream.HealthPath) {
		t.Fatalf("want startup error naming %s, got %v", stream.HealthPath, err)
	}
}

// A contract endpoint (generated code registers via Route, with a method) declaring /health also fails startup.
func TestHealth_ContractRouteConflictFailsStartup(t *testing.T) {
	_, err := stream.NewHTTPServer(&conf.Server_HTTP{Addr: "127.0.0.1:0"},
		func(s *khttp.Server) {
			s.Route("/").GET(stream.HealthPath, func(khttp.Context) error { return nil })
		}, mw.Default(nil))
	if err == nil || !strings.Contains(err.Error(), stream.HealthPath) {
		t.Fatalf("want startup error naming %s, got %v", stream.HealthPath, err)
	}
}
