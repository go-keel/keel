package api_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/middleware/auth"
	"github.com/go-keel/keel/middleware/metadata"
	"github.com/go-keel/keel/mw"
	"github.com/go-keel/keel/pkg/jwt"
	"github.com/go-keel/keel/server/api"
	"github.com/go-kratos/kratos/v3/encoding"
	"github.com/go-kratos/kratos/v3/middleware"
	kgrpc "github.com/go-kratos/kratos/v3/transport/grpc"
	khttp "github.com/go-kratos/kratos/v3/transport/http"
	"github.com/gorilla/websocket"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	grpcmd "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const rawAuthSecret = "raw-route-secret"

func signRaw(t *testing.T, sub string) string {
	t.Helper()
	signer, err := jwt.NewHS256Signer([]byte(rawAuthSecret))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	token, err := signer.Sign(jwt.NewClaims(sub, time.Hour))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return token
}

// startRawAuthServer starts a real HTTP server: /required goes through RequireLogin(),
// /anon through Anonymous(), and both write claims["sub"] (or "anon" if missing) back to the response body.
func startRawAuthServer(t *testing.T, authCfg *conf.Server_Auth) string {
	t.Helper()
	srvT, err := api.NewHTTPServer(&conf.Server_HTTP{
		Addr: "127.0.0.1:0",
		Auth: authCfg,
	}, func(s *khttp.Server) {}, mw.Default(nil), func(sc *api.StreamContext) {
		sc.RequireLogin().HandleFunc("/required", func(w http.ResponseWriter, r *http.Request) {
			claims, _ := auth.FromContext(r.Context())
			sub, _ := claims["sub"].(string)
			_, _ = io.WriteString(w, sub)
		})
		sc.Anonymous().HandleFunc("/anon", func(w http.ResponseWriter, r *http.Request) {
			if claims, ok := auth.FromContext(r.Context()); ok {
				sub, _ := claims["sub"].(string)
				_, _ = io.WriteString(w, sub)
				return
			}
			_, _ = io.WriteString(w, "anon")
		})
	})
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

func getBody(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

func rawAuthConfig() *conf.Server_Auth {
	return &conf.Server_Auth{Enabled: true, Algorithm: "HS256", JwtSecret: rawAuthSecret}
}

// RequireLogin without a credential: the same structured error as contract endpoints (the same
// khttp.DefaultErrorEncoder), with message "not signed in", not a plain-text 401 assembled by the raw route.
func TestRawRoute_RequireLogin_MissingCredential(t *testing.T) {
	base := startRawAuthServer(t, rawAuthConfig())
	req, _ := http.NewRequest(http.MethodGet, base+"/required", nil)
	code, body := getBody(t, req)
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
	if !strings.Contains(body, "not signed in") || !strings.Contains(body, "UNAUTHORIZED") {
		t.Fatalf("body = %q, want structured error mentioning missing token", body)
	}
}

// RequireLogin with an invalid credential: a message different from "missing credential", still without leaking the specific verification failure.
func TestRawRoute_RequireLogin_InvalidCredential(t *testing.T) {
	base := startRawAuthServer(t, rawAuthConfig())
	req, _ := http.NewRequest(http.MethodGet, base+"/required", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	code, body := getBody(t, req)
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
	if !strings.Contains(body, "invalid token") {
		t.Fatalf("body = %q, want structured error mentioning invalid token", body)
	}
}

func TestRawRoute_RequireLogin_ValidCredential(t *testing.T) {
	base := startRawAuthServer(t, rawAuthConfig())
	req, _ := http.NewRequest(http.MethodGet, base+"/required", nil)
	req.Header.Set("Authorization", "Bearer "+signRaw(t, "u1"))
	code, body := getBody(t, req)
	if code != http.StatusOK || body != "u1" {
		t.Fatalf("status=%d body=%q, want 200/u1", code, body)
	}
}

// The header wins over the cookie, consistent with contract endpoints (the unary chain): a caller that explicitly
// sends the header must not be overridden by a cookie the browser sends along.
func TestRawRoute_RequireLogin_HeaderWinsOverCookie(t *testing.T) {
	cfg := rawAuthConfig()
	cfg.CookieName = "sess"
	base := startRawAuthServer(t, cfg)

	req, _ := http.NewRequest(http.MethodGet, base+"/required", nil)
	req.Header.Set("Authorization", "Bearer "+signRaw(t, "header-user"))
	req.AddCookie(&http.Cookie{Name: "sess", Value: signRaw(t, "cookie-user")})

	code, body := getBody(t, req)
	if code != http.StatusOK || body != "header-user" {
		t.Fatalf("status=%d body=%q, want 200/header-user (header should win over cookie)", code, body)
	}
}

// Anonymous without a credential: let through, no identity written.
func TestRawRoute_Anonymous_NoCredential(t *testing.T) {
	base := startRawAuthServer(t, rawAuthConfig())
	req, _ := http.NewRequest(http.MethodGet, base+"/anon", nil)
	code, body := getBody(t, req)
	if code != http.StatusOK || body != "anon" {
		t.Fatalf("status=%d body=%q, want 200/anon", code, body)
	}
}

// Anonymous with an invalid credential: still let through (consistent with the contract endpoint allow-list
// tolerating invalid tokens), just no identity written.
func TestRawRoute_Anonymous_InvalidCredentialStillPasses(t *testing.T) {
	base := startRawAuthServer(t, rawAuthConfig())
	req, _ := http.NewRequest(http.MethodGet, base+"/anon", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	code, body := getBody(t, req)
	if code != http.StatusOK || body != "anon" {
		t.Fatalf("status=%d body=%q, want 200/anon (an invalid credential must not block an anonymous route)", code, body)
	}
}

// An anonymous route with a valid credential still writes the identity.
func TestRawRoute_Anonymous_ValidCredentialWritesIdentity(t *testing.T) {
	base := startRawAuthServer(t, rawAuthConfig())
	req, _ := http.NewRequest(http.MethodGet, base+"/anon", nil)
	req.Header.Set("Authorization", "Bearer "+signRaw(t, "u2"))
	code, body := getBody(t, req)
	if code != http.StatusOK || body != "u2" {
		t.Fatalf("status=%d body=%q, want 200/u2 (a valid credential should write the identity)", code, body)
	}
}

// One of the three server-construction errors: login declared (RequireLogin) but the server has no authentication;
// there is no verifier to check with, so the declaration itself is wrong.
func TestNewHTTPServer_RequireLoginWithoutServerAuth(t *testing.T) {
	_, err := api.NewHTTPServer(&conf.Server_HTTP{Addr: "127.0.0.1:0"},
		func(s *khttp.Server) {}, mw.Default(nil),
		func(sc *api.StreamContext) {
			sc.RequireLogin().HandleFunc("/x", func(w http.ResponseWriter, r *http.Request) {})
		})
	if err == nil {
		t.Fatal("expected error: RequireLogin declared but server has no auth configured")
	}
}

// Server-construction error two: the chain has no auth slot at all (Minimal() does not reserve one), yet config enables authentication.
func TestNewHTTPServer_NoAuthSlotInChain(t *testing.T) {
	_, err := api.NewHTTPServer(&conf.Server_HTTP{
		Addr: "127.0.0.1:0",
		Auth: rawAuthConfig(),
	}, func(s *khttp.Server) {}, mw.Minimal())
	if err == nil {
		t.Fatal("expected error: chain has no auth slot")
	}
}

func TestNewGRPCServer_NoAuthSlotInChain(t *testing.T) {
	_, err := api.NewGRPCServer(&conf.Server_GRPC{
		Addr: "127.0.0.1:0",
		Auth: rawAuthConfig(),
	}, func(*kgrpc.Server) {}, mw.Minimal())
	if err == nil {
		t.Fatal("expected error: chain has no auth slot")
	}
}

// startIPServer starts a server via the real NewHTTPServer; /ip runs through the middleware chain and writes GetRemoteIP back.
func startIPServer(t *testing.T, chain *mw.Chain, clientIP *conf.Server_ClientIP) string {
	t.Helper()
	srvT, err := api.NewHTTPServer(&conf.Server_HTTP{Addr: "127.0.0.1:0", ClientIp: clientIP}, func(s *khttp.Server) {
		s.Route("/").GET("/ip", func(ctx khttp.Context) error {
			out, err := ctx.Middleware(func(c context.Context, _ any) (any, error) {
				return metadata.GetRemoteIP(c), nil
			})(ctx, nil)
			if err != nil {
				return err
			}
			return ctx.String(http.StatusOK, out.(string))
		})
	}, chain)
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

// One base chain reused by two servers, each taking the client IP with its own client_ip config: test requests all
// come from the local host; the default config trusts the local host and reads X-Forwarded-For, while the one
// trusting only 10.0.0.0/8 no longer trusts the local host.
func TestNewHTTPServer_ClientIPPerServer(t *testing.T) {
	base := mw.Default(nil)
	loopback := startIPServer(t, base, nil)
	lan := startIPServer(t, base, &conf.Server_ClientIP{TrustedProxies: []string{"10.0.0.0/8"}})
	ignore := startIPServer(t, base, &conf.Server_ClientIP{IgnoreForwardedFor: true})

	for _, tt := range []struct {
		name, base, want string
	}{
		{"default trusts only the local host", loopback, "1.1.1.1"},
		{"list replaces the default", lan, "127.0.0.1"},
		{"ignore_forwarded_for", ignore, "127.0.0.1"},
	} {
		req, _ := http.NewRequest(http.MethodGet, tt.base+"/ip", nil)
		req.Header.Add("X-Forwarded-For", "6.6.6.6")
		req.Header.Add("X-Forwarded-For", "1.1.1.1")
		req.Header.Set("X-Real-IP", "5.5.5.5")
		req.Header.Set(metadata.KeyRemoteAddress, "9.9.9.9")
		if code, body := getBody(t, req); code != http.StatusOK || body != tt.want {
			t.Errorf("%s: got %d %q, want %q", tt.name, code, body, tt.want)
		}
	}
}

// WithTrustedMetadata semantics unchanged: an upstream x-keel-remote-address is kept when present, otherwise the
// value this server computes from its config is used.
func TestNewHTTPServer_ClientIPTrustIncoming(t *testing.T) {
	chain := mw.Default(nil)
	mw.WithTrustedMetadata(chain)
	base := startIPServer(t, chain, nil)
	for _, tt := range []struct {
		name, upstream, want string
	}{
		{"upstream value kept", "9.9.9.9", "9.9.9.9"},
		{"no upstream value uses this hop", "", "1.1.1.1"},
	} {
		req, _ := http.NewRequest(http.MethodGet, base+"/ip", nil)
		req.Header.Set("X-Forwarded-For", "1.1.1.1")
		if tt.upstream != "" {
			req.Header.Set(metadata.KeyRemoteAddress, tt.upstream)
		}
		if code, body := getBody(t, req); code != http.StatusOK || body != tt.want {
			t.Errorf("%s: got %d %q, want %q", tt.name, code, body, tt.want)
		}
	}
}

// GetRemoteIP outside the chain: raw routes and filters mounted via StreamContext.Filter both get the address computed
// with this port's client_ip config. The list trusts only 10.0.0.0/8 and test requests come from the local host, so
// getting 127.0.0.1 shows this port's config is used; the default rule (trust the local host) would return 1.1.1.1
// from X-Forwarded-For.
func TestNewHTTPServer_ClientIPOutsideChain(t *testing.T) {
	srvT, err := api.NewHTTPServer(&conf.Server_HTTP{
		Addr:     "127.0.0.1:0",
		ClientIp: &conf.Server_ClientIP{TrustedProxies: []string{"10.0.0.0/8"}},
	}, func(*khttp.Server) {}, mw.Default(nil), func(sc *api.StreamContext) {
		sc.Filter(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Filter-IP", metadata.GetRemoteIP(r.Context()))
				next.ServeHTTP(w, r)
			})
		})
		sc.Anonymous().HandleFunc("/raw", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(metadata.GetRemoteIP(r.Context())))
		})
	})
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

	req, _ := http.NewRequest(http.MethodGet, "http://"+ep.Host+"/raw", nil)
	req.Header.Set("X-Forwarded-For", "1.1.1.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if got := resp.Header.Get("X-Filter-IP"); got != "127.0.0.1" {
		t.Errorf("filter: got %q, want 127.0.0.1", got)
	}
	if string(body) != "127.0.0.1" {
		t.Errorf("raw route: got %q, want 127.0.0.1", body)
	}
}

func TestStreamContext_FilterNil(t *testing.T) {
	_, err := api.NewHTTPServer(&conf.Server_HTTP{Addr: "127.0.0.1:0"}, func(*khttp.Server) {}, mw.Default(nil),
		func(sc *api.StreamContext) { sc.Filter(nil) })
	if err == nil {
		t.Fatal("want error for nil filter")
	}
}

func TestCodecContract_JSON(t *testing.T) {
	codec := encoding.GetCodec("json")

	// Timestamp → RFC3339
	ts := timestamppb.New(time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC))
	data, err := codec.Marshal(ts)
	if err != nil {
		t.Fatalf("marshal timestamp: %v", err)
	}
	if !strings.Contains(string(data), "2026-07-03T10:00:00Z") {
		t.Errorf("timestamp should be RFC3339, got %s", data)
	}

	// int64 → JSON string (JS precision)
	data, err = codec.Marshal(wrapperspb.Int64(9007199254740993))
	if err != nil {
		t.Fatalf("marshal int64: %v", err)
	}
	if !strings.Contains(string(data), `"9007199254740993"`) {
		t.Errorf("int64 should be a JSON string, got %s", data)
	}

	// non-proto → stdlib json
	data, err = codec.Marshal(map[string]int64{"n": 42})
	if err != nil {
		t.Fatalf("marshal map: %v", err)
	}
	if string(data) != `{"n":42}` {
		t.Errorf("non-proto should use stdlib json, got %s", data)
	}

	// Unknown fields are discarded (DiscardUnknown); int64 accepts the string form
	var v wrapperspb.Int64Value
	if err := codec.Unmarshal([]byte(`"123"`), &v); err != nil {
		t.Fatalf("unmarshal string int64: %v", err)
	}
	if v.GetValue() != 123 {
		t.Errorf("value = %d, want 123", v.GetValue())
	}
	var ts2 timestamppb.Timestamp
	if err := codec.Unmarshal([]byte(`"2026-07-03T10:00:00Z"`), &ts2); err != nil {
		t.Fatalf("unmarshal timestamp: %v", err)
	}

	// Unknown fields are discarded (DiscardUnknown): if the upstream codec drops this option, this reports
	// "unknown field" instead of silently ignoring it
	var lg conf.Logger
	if err := codec.Unmarshal([]byte(`{"level":"warn","not_a_real_field":1}`), &lg); err != nil {
		t.Fatalf("unknown field should be discarded, got error: %v", err)
	}
	if lg.GetLevel() != "warn" {
		t.Errorf("level = %q, want %q", lg.GetLevel(), "warn")
	}
}

const testSecret = "integration-test-secret"

// newContractServer starts an HTTP server via the real NewHTTPServer: authentication is built from cfg.Auth
// (HS256, no anonymous allow-list), chain = the auth slot reserved by mw.Default + a recording middleware.
func newContractServer(t *testing.T, hits *atomic.Int32) (base string) {
	t.Helper()

	chain := mw.Default(nil)
	// record sits at the end of the chain (after auth): it counts only requests that passed authentication into the business
	chain.InsertAfter(mw.SlotValidate, func(h middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			hits.Add(1)
			return h(ctx, req)
		}
	}, "record")

	cfg := &conf.Server_HTTP{
		Addr: "127.0.0.1:0",
		Auth: &conf.Server_Auth{
			Enabled:   true,
			Algorithm: "HS256",
			JwtSecret: testSecret,
		},
	}

	srvT, err := api.NewHTTPServer(cfg, registerContractRoutes, chain)
	if err != nil {
		t.Fatalf("NewHTTPServer: %v", err)
	}
	srv := srvT.(*khttp.Server)

	endpoint, err := srv.Endpoint()
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	go func() { _ = srv.Start(context.Background()) }()
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	return "http://" + endpoint.Host
}

// registerContractRoutes hand-writes routes of the four types in the shape of generated stubs (1a unary / 1b SSE /
// 1c WebSocket / 1d HttpBody), passed by newContractServer to NewHTTPServer as the register callback.
func registerContractRoutes(srv *khttp.Server) {
	// 1a unary (same shape as the generated stub)
	srv.Route("/").GET("/unary", func(ctx khttp.Context) error {
		khttp.SetOperation(ctx, "/test/Unary")
		h := ctx.Middleware(func(c context.Context, req any) (any, error) {
			return wrapperspb.Int64(9007199254740993), nil
		})
		out, err := h(ctx, &struct{}{})
		if err != nil {
			return err
		}
		return ctx.Result(200, out)
	})

	// 1b SSE (same shape as the server-stream generated stub)
	srv.Route("/").GET("/sse", func(ctx khttp.Context) error {
		stream := khttp.NewServerSentEventServerStream(ctx)
		khttp.SetOperation(ctx, "/test/Watch")
		h := ctx.Middleware(func(c context.Context, req any) (any, error) {
			stream.SetContext(c)
			for i := int64(1); i <= 2; i++ {
				if err := stream.Send(wrapperspb.Int64(9007199254740990 + i)); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		_, err := h(ctx, &struct{}{})
		return stream.Close(err)
	})

	// 1c WebSocket (same shape as the bidi-stream generated stub: echo one message)
	srv.Route("/").GET("/ws", func(ctx khttp.Context) error {
		stream, err := khttp.NewWebSocketServerStream(ctx)
		if err != nil {
			return err
		}
		khttp.SetOperation(ctx, "/test/Sync")
		h := ctx.Middleware(func(c context.Context, req any) (any, error) {
			stream.SetContext(c)
			var in wrapperspb.Int64Value
			if err := stream.Recv(&in); err != nil {
				return nil, err
			}
			return nil, stream.Send(wrapperspb.Int64(in.GetValue() + 1))
		})
		_, err = h(ctx, nil)
		return stream.Close(err)
	})

	// 1d HttpBody (same shape as the unary generated stub, returns raw bytes)
	srv.Route("/").GET("/export", func(ctx khttp.Context) error {
		khttp.SetOperation(ctx, "/test/Export")
		h := ctx.Middleware(func(c context.Context, req any) (any, error) {
			return &httpbody.HttpBody{ContentType: "text/csv", Data: []byte("id,title\n1,a\n")}, nil
		})
		out, err := h(ctx, &struct{}{})
		if err != nil {
			return err
		}
		return ctx.Result(200, out)
	})
}

func signTestToken(t *testing.T) string {
	t.Helper()
	signer, err := jwt.NewHS256Signer([]byte(testSecret))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	token, err := signer.Sign(jwt.NewClaims("tester", time.Hour))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return token
}

func authedGet(t *testing.T, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+signTestToken(t))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func TestContract_MiddlewareRunsOnAllFourTypes(t *testing.T) {
	var hits atomic.Int32
	base := newContractServer(t, &hits)

	// 1a
	resp := authedGet(t, base+"/unary")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"9007199254740993"`) {
		t.Fatalf("unary: status=%d body=%s (int64 should be a protojson string)", resp.StatusCode, body)
	}

	// 1b SSE
	resp = authedGet(t, base+"/sse")
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("sse content-type = %q", got)
	}
	if !strings.Contains(string(body), `"9007199254740991"`) || !strings.Contains(string(body), "data:") {
		t.Fatalf("sse frames = %s (frames should be protojson)", body)
	}

	// 1c WebSocket
	h := http.Header{}
	h.Set("Authorization", "Bearer "+signTestToken(t))
	conn, wsResp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/ws", h)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer func() { _ = wsResp.Body.Close() }()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`"41"`)); err != nil {
		t.Fatalf("ws write: %v", err)
	}
	_, frame, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	_ = conn.Close()
	if !strings.Contains(string(frame), `"42"`) {
		t.Fatalf("ws echo = %s (should be the protojson int64 string 42)", frame)
	}

	// 1d HttpBody
	resp = authedGet(t, base+"/export")
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/csv" || string(body) != "id,title\n1,a\n" {
		t.Fatalf("httpbody: ct=%q body=%q", resp.Header.Get("Content-Type"), body)
	}

	// All four types must pass through the chain's middleware
	if got := hits.Load(); got != 4 {
		t.Fatalf("middleware hits = %d, want 4", got)
	}
}

func TestContract_AuthRejectsAllFourTypesBeforeStream(t *testing.T) {
	var hits atomic.Int32
	base := newContractServer(t, &hits)

	// Missing credential: no Authorization header.
	for _, path := range []string{"/unary", "/sse", "/export"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("%s without token: status = %d, want 401", path, resp.StatusCode)
		}
		if !strings.Contains(string(body), "not signed in") {
			t.Fatalf("%s without token: body = %s, want message about missing token", path, body)
		}
	}

	// Invalid credential: well-formed but the signature does not match. Compare with the missing-credential branch
	// above: both are 401, but the message must differ, otherwise clients cannot tell "not signed in" from "broken
	// token" (the specific verification failure still does not leak; only this one distinction is kept).
	for _, path := range []string{"/unary", "/sse", "/export"} {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		req.Header.Set("Authorization", "Bearer not-a-real-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s with invalid token: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("%s with invalid token: status = %d, want 401", path, resp.StatusCode)
		}
		if !strings.Contains(string(body), "invalid token") {
			t.Fatalf("%s with invalid token: body = %s, want message about invalid token", path, body)
		}
		if strings.Contains(string(body), "not signed in") {
			t.Fatalf("%s with invalid token: body = %s, must not reuse the missing-credential message", path, body)
		}
	}

	// The established WebSocket semantics (kratos-generated stubs Upgrade first, then run the middleware):
	// the handshake succeeds (101), and an auth failure is delivered via a control frame ("\x1e error:...") + 1011 close.
	// Clients must handle a first-frame error; see section 1c of docs/http.md.
	conn, wsResp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("ws handshake should succeed (upgrade happens before middleware): %v", err)
	}
	defer func() { _ = wsResp.Body.Close() }()
	_, frame, err := conn.ReadMessage()
	_ = conn.Close()
	if err == nil {
		// The first frame should be an error control frame (\x1e prefix)
		if !strings.HasPrefix(string(frame), "\x1e") || !strings.Contains(string(frame), "UNAUTHORIZED") {
			t.Fatalf("ws first frame = %q, want control error frame", frame)
		}
	} else if !websocket.IsCloseError(err, websocket.CloseInternalServerErr) {
		t.Fatalf("ws read: %v, want control error frame or 1011 close", err)
	}

	if got := hits.Load(); got != 0 {
		t.Fatalf("the handler after the business middleware must not run, hits = %d", got)
	}
}

const streamMethod = "/keel.test.v1.Streamer/Echo"

type ctxKey struct{}

// newStreamServer starts a real gRPC server (authentication built from cfg.Auth, HS256) and registers a hand-written
// bidirectional stream ServiceDesc (echoing every message received); chain = the auth slot reserved by mw.Default +
// record. record sits at the end of the chain (after auth) and counts only calls that passed authentication into the
// business; it also puts a marker in the context so tests can assert the context injected by middleware reaches the handler.
func newStreamServer(t *testing.T, hits *atomic.Int32, handlerCtx chan<- context.Context) string {
	t.Helper()

	chain := mw.Default(nil)
	chain.InsertAfter(mw.SlotValidate, func(h middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			hits.Add(1)
			return h(context.WithValue(ctx, ctxKey{}, "injected"), req)
		}
	}, "record")

	desc := grpc.ServiceDesc{
		ServiceName: "keel.test.v1.Streamer",
		HandlerType: (*any)(nil),
		Streams: []grpc.StreamDesc{{
			StreamName:    "Echo",
			ServerStreams: true,
			ClientStreams: true,
			Handler: func(_ any, ss grpc.ServerStream) error {
				handlerCtx <- ss.Context()
				for {
					var m wrapperspb.StringValue
					switch err := ss.RecvMsg(&m); {
					case errors.Is(err, io.EOF):
						return nil
					case err != nil:
						return err
					}
					if err := ss.SendMsg(&m); err != nil {
						return err
					}
				}
			},
		}},
	}

	cfg := &conf.Server_GRPC{
		Addr: "127.0.0.1:0",
		Auth: &conf.Server_Auth{
			Enabled:   true,
			Algorithm: "HS256",
			JwtSecret: testSecret,
		},
	}
	srv, err := api.NewGRPCServer(cfg, func(s *kgrpc.Server) {
		s.RegisterService(&desc, struct{}{})
	}, chain)
	if err != nil {
		t.Fatalf("NewGRPCServer: %v", err)
	}
	ks := srv.(*kgrpc.Server)
	ep, err := ks.Endpoint()
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	go func() { _ = ks.Start(context.Background()) }()
	t.Cleanup(func() { _ = ks.Stop(context.Background()) })
	return ep.Host
}

func dialStream(t *testing.T, addr, token string) grpc.ClientStream {
	t.Helper()
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if token != "" {
		ctx = grpcmd.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	}
	st, err := cc.NewStream(ctx, &grpc.StreamDesc{
		StreamName:    "Echo",
		ServerStreams: true,
		ClientStreams: true,
	}, streamMethod)
	if err != nil {
		t.Fatalf("new stream: %v", err)
	}
	return st
}

// The whole chain wraps the whole stream: 3 round trips, the chain runs once. Mistakenly using kratos's
// StreamMiddleware (per message) would count 6 here.
func TestGRPCStreamChainRunsOncePerStream(t *testing.T) {
	var hits atomic.Int32
	handlerCtx := make(chan context.Context, 1)
	addr := newStreamServer(t, &hits, handlerCtx)

	st := dialStream(t, addr, signTestToken(t))
	for i := 0; i < 3; i++ {
		if err := st.SendMsg(wrapperspb.String("ping")); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		var out wrapperspb.StringValue
		if err := st.RecvMsg(&out); err != nil {
			t.Fatalf("recv %d: %v", i, err)
		}
		if out.GetValue() != "ping" {
			t.Fatalf("recv %d: got %q", i, out.GetValue())
		}
	}
	if err := st.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}

	if got := hits.Load(); got != 1 {
		t.Fatalf("middleware hits = %d, want 1 (the whole chain should run once per stream)", got)
	}
	if v := (<-handlerCtx).Value(ctxKey{}); v != "injected" {
		t.Fatalf("handler context value = %v, want injected (the context injected by the chain did not reach the handler)", v)
	}
}

// Auth rejection happens at stream establishment: a stream without a token gets Unauthenticated on its first
// send/receive, and the handler is never called.
// Auth rejection happens at stream establishment, and a missing credential and an invalid one get different messages
// (compare TestContract_AuthRejectsAllFourTypesBeforeStream in contract_integration_test.go: the unary and streaming
// paths must distinguish their error messages consistently, otherwise clients cannot tell "not signed in" from
// "broken token").
func TestGRPCStreamAuthRejectsAtOpen(t *testing.T) {
	var hits atomic.Int32
	handlerCtx := make(chan context.Context, 1)
	addr := newStreamServer(t, &hits, handlerCtx)

	assertRejected := func(t *testing.T, token, wantMsg string) {
		t.Helper()
		st := dialStream(t, addr, token)
		_ = st.SendMsg(wrapperspb.String("ping"))
		var out wrapperspb.StringValue
		err := st.RecvMsg(&out)
		if code := status.Code(err); code != codes.Unauthenticated {
			t.Fatalf("recv error = %v (code %v), want Unauthenticated", err, code)
		}
		if msg := status.Convert(err).Message(); msg != wantMsg {
			t.Fatalf("message = %q, want %q", msg, wantMsg)
		}
	}

	t.Run("missing", func(t *testing.T) {
		assertRejected(t, "", "not signed in")
	})
	t.Run("invalid", func(t *testing.T) {
		assertRejected(t, "not-a-real-token", "invalid token")
	})

	if got := hits.Load(); got != 0 {
		t.Fatalf("middleware hits = %d, want 0 (middleware after auth must not run)", got)
	}
	if len(handlerCtx) != 0 {
		t.Fatal("handler was called; auth did not reject at stream establishment")
	}
}

// streamChain also wraps the framework's own streaming methods, of which grpc.health.v1.Health/Watch is the only one
// real operations use. Under the default chain it must keep working and still push NOT_SERVING on Stop; this guards
// "plugging streaming RPCs into the middleware chain does not break the framework's own services". The health check
// itself is built into kratos; its semantics are not tested here, only that the chain does not break it.
func TestGRPCStreamKeepsBuiltinHealthWorking(t *testing.T) {
	chain := mw.Default(nil)
	srv, err := api.NewGRPCServer(&conf.Server_GRPC{Addr: "127.0.0.1:0"}, func(*kgrpc.Server) {}, chain)
	if err != nil {
		t.Fatalf("NewGRPCServer: %v", err)
	}
	ks := srv.(*kgrpc.Server)
	ep, err := ks.Endpoint()
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	go func() { _ = ks.Start(context.Background()) }()

	cc, err := grpc.NewClient(ep.Host, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	cli := grpc_health_v1.NewHealthClient(cc)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	resp, err := cli.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("Check = %v, %v; want SERVING", resp.GetStatus(), err)
	}

	st, err := cli.Watch(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Watch stream: %v", err)
	}
	first, err := st.Recv()
	if err != nil || first.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("Watch first frame = %v, %v; want SERVING", first.GetStatus(), err)
	}

	go func() { _ = ks.Stop(context.Background()) }()
	second, err := st.Recv()
	if err != nil || second.GetStatus() != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("push after Stop = %v, %v; want NOT_SERVING", second.GetStatus(), err)
	}
}

// startProtoEchoServer starts a NewHTTPServer and registers a native handler that echoes the request's protocol version.
func startProtoEchoServer(t *testing.T) string {
	t.Helper()
	srv, err := api.NewHTTPServer(&conf.Server_HTTP{Addr: "127.0.0.1:0"}, func(s *khttp.Server) {
		s.HandleFunc("/proto", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, r.Proto)
		})
	}, mw.Minimal())
	if err != nil {
		t.Fatalf("NewHTTPServer: %v", err)
	}
	ks := srv.(*khttp.Server)
	ep, err := ks.Endpoint()
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	go func() { _ = ks.Start(context.Background()) }()
	t.Cleanup(func() { _ = ks.Stop(context.Background()) })
	return "http://" + ep.Host + "/proto"
}

func getProto(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func TestNewHTTPServer_H2CPriorKnowledge(t *testing.T) {
	url := startProtoEchoServer(t)

	var protocols http.Protocols
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)
	h2cClient := &http.Client{Transport: &http.Transport{Protocols: &protocols}}
	if got := getProto(t, h2cClient, url); got != "HTTP/2.0" {
		t.Fatalf("h2c client proto = %q, want HTTP/2.0", got)
	}
}

func TestNewHTTPServer_HTTP1Unaffected(t *testing.T) {
	url := startProtoEchoServer(t)
	if got := getProto(t, &http.Client{}, url); got != "HTTP/1.1" {
		t.Fatalf("http/1.1 client proto = %q, want HTTP/1.1", got)
	}
}
