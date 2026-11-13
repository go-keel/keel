package api

import (
	// kratos v3 split the JSON codec: the built-in "json" codec is pure stdlib and produces machine formats for proto
	// messages (Timestamp→{seconds,nanos}, enum→number, int64→number, a precision risk on the JS side). The industry
	// convention for proto-first APIs (grpc-gateway, Google AIP) is protobuf JSON semantics (RFC3339 times, enum
	// names, int64 strings).
	//
	// Keel blank-imports kratos's official contrib JSON codec to replace the global "json" codec with the behaviour
	// "proto.Message uses protojson, everything else stdlib" (MarshalOptions match v3 encoding/protojson:
	// EmitUnpopulated + DiscardUnknown). Unary requests/responses, error encoding and in-frame encoding of streaming
	// RPCs share the same codec. Special responses such as HttpBody / redirect are handled natively by kratos's
	// default encoder.
	//
	// Note: a process can register only one "json" codec; business code must not register another.
	_ "github.com/go-kratos/kratos/contrib/encoding/json/v3"

	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-kratos/kratos/v3/middleware"
	kgrpc "github.com/go-kratos/kratos/v3/transport/grpc"
	khttp "github.com/go-kratos/kratos/v3/transport/http"
	"google.golang.org/grpc"

	"github.com/go-keel/keel"
	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/internal/authslot"
	"github.com/go-keel/keel/internal/clientip"
	"github.com/go-keel/keel/internal/errs"
	"github.com/go-keel/keel/mw"
	internalauth "github.com/go-keel/keel/server/api/internal/auth"

	// kratos v3 split the JSON codec: "json" = pure stdlib (the default encoding), and "protojson" must be registered
	// explicitly. It is registered here to support explicit negotiation with Accept/Content-Type:
	// application/protojson; kratos's streaming endpoints depend on it too.
	_ "github.com/go-kratos/kratos/v3/encoding/protojson"
)

// HTTPServer is an alias of the kratos HTTP server, not a new type: it is a struct, and defining a new type would
// lose all its methods such as Handle/HandleFunc/HandlePrefix/WalkRoute, which StreamContext and the register
// callbacks need.
type HTTPServer = khttp.Server

// GRPCServer is likewise an alias of the kratos gRPC server.
type GRPCServer = kgrpc.Server

type GRPCServerRegister func(server *GRPCServer)
type HTTPServerRegister func(server *HTTPServer)

// NewHTTPServer creates an HTTP server from the given config and middleware Chain.
// Authentication is built only once from cfg.GetAuth() and used both in the chain's auth slot (contract endpoints)
// and in raw routes registered by streamRegister (via StreamContext.RequireLogin/Anonymous); the two paths share one
// verifier and behave identically case by case. streamRegister is optional; use it
// to register SSE/download handlers on the same server.
func NewHTTPServer(
	cfg *conf.Server_HTTP,
	register HTTPServerRegister,
	chain *mw.Chain,
	streamRegister ...StreamRegister,
) (keel.Server, error) {
	// A wrong config section name (cfg.Server.Http["xxx"] does not exist) yields nil; values such as addr are
	// checked by the rules on conf.proto when config loads.
	if cfg == nil {
		return nil, errs.Rules.New("api.http-config-missing", "NewHTTPServer was given a nil config, usually because server.http.<name> has no section with that name", "add the section under server.http in the config, or pass the name that exists")
	}

	built, err := internalauth.Build(cfg.GetAuth())
	if err != nil {
		return nil, errs.While("api: build auth", err)
	}

	// Clone() rather than modifying the caller's chain in place: when one base chain is reused by several servers,
	// their auth slots do not interfere.
	chain = chain.Clone()
	if built != nil {
		authslot.Fill(chain, built.Middleware)
	}
	if err := chain.Err(); err != nil {
		return nil, errs.While("api: build middleware chain", err)
	}

	// The client IP is computed with this server's client_ip config before routing and put in the request context;
	// the metadata slot reads it and writes x-keel-remote-address. It is not a middleware on the chain: the base chain
	// is reused by several servers and cannot tell which port a request came from, so per-port trusted lists could
	// not be told apart. It must wrap the filters mounted via StreamContext.Filter, so it is installed after all
	// callbacks have run; see the end.
	resolver, err := clientip.New(cfg.GetClientIp())
	if err != nil {
		return nil, err
	}

	opts := []khttp.ServerOption{
		khttp.Address(cfg.GetAddr()),
		khttp.Middleware(chain.Build()...),
	}
	if cfg.GetTimeout() != nil {
		opts = append(opts, khttp.Timeout(cfg.GetTimeout().AsDuration()))
	}

	srv := khttp.NewServer(opts...)

	// The plaintext port accepts both HTTP/1.1 and prior-knowledge HTTP/2 (h2c): the same port sniffs the connection
	// preface, HTTP/1.1 clients are unaffected, and with TLS ALPN still negotiates.
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)
	srv.Protocols = &protocols

	// The liveness check is registered before every business route; see registerHealth for why.
	registerHealth(srv)

	register(srv)

	_ = srv.WalkRoute(func(info khttp.RouteInfo) error {
		slog.Info("http route registered",
			"method", info.Method,
			"path", info.Path,
		)
		return nil
	})

	var filters []func(http.Handler) http.Handler
	if len(streamRegister) > 0 {
		sc := &StreamContext{server: srv}
		if built != nil {
			sc.requireLogin = built.RequireLogin
			sc.anonymous = built.Anonymous
		}
		for _, fn := range streamRegister {
			if fn != nil {
				fn(sc)
			}
		}
		if sc.err != nil {
			return nil, errs.While("api: register raw routes", sc.err)
		}
		filters = sc.filters
	}

	// The order is fixed: compute client IP → filters mounted via StreamContext.Filter (first mounted is outermost) → routing.
	// The client IP step must run first, otherwise GetRemoteIP in those filters cannot get the value computed with this port's config.
	h := srv.Handler
	for i := len(filters) - 1; i >= 0; i-- {
		h = filters[i](h)
	}
	srv.Handler = resolver.Filter(h)

	if err := checkHealthConflict(srv); err != nil {
		return nil, fmt.Errorf("api: %w", err)
	}

	return srv, nil
}

// NewGRPCServer creates a gRPC server from the given config and middleware Chain.
// Authentication is built only once from cfg.GetAuth() and filled into the chain's auth slot; unary and streaming
// (at stream establishment) share the same chain, see streamChain.
func NewGRPCServer(
	cfg *conf.Server_GRPC,
	register GRPCServerRegister,
	chain *mw.Chain,
) (keel.Server, error) {
	if cfg == nil {
		return nil, errs.Rules.New("api.grpc-config-missing", "NewGRPCServer was given a nil config, usually because server.grpc.<name> has no section with that name", "add the section under server.grpc in the config, or pass the name that exists")
	}

	built, err := internalauth.Build(cfg.GetAuth())
	if err != nil {
		return nil, errs.While("api: build auth", err)
	}

	chain = chain.Clone()
	if built != nil {
		authslot.Fill(chain, built.Middleware)
	}
	if err := chain.Err(); err != nil {
		return nil, errs.While("api: build middleware chain", err)
	}

	opts := []kgrpc.ServerOption{
		kgrpc.Address(cfg.GetAddr()),
		kgrpc.Middleware(chain.Build()...),
		kgrpc.StreamInterceptor(streamChain(chain)),
	}
	if cfg.GetTimeout() != nil {
		opts = append(opts, kgrpc.Timeout(cfg.GetTimeout().AsDuration()))
	}

	srv := kgrpc.NewServer(opts...)
	register(srv)
	return srv, nil
}

// streamChain makes gRPC streaming RPCs run the same middleware chain as unary, with semantics aligned with
// SSE/WebSocket on the HTTP side: the whole chain wraps the stream's whole lifetime, and at stream establishment req
// is nil and reply always nil, which is exactly the shape of kratos's own generated WebSocket stubs. So auth rejects
// at stream establishment, the tracing span covers the whole stream, the metrics duration is the connection
// duration, and logging produces one completion record per stream.
//
// It does not use kratos's StreamMiddleware: that fires per message on SendMsg/RecvMsg, counting messages as
// requests and opening a span per message, unlike the HTTP side; and its call at the stream establishment point
// (middleware.Chain(next...)(h) in transport/grpc/interceptor.go) builds the chain without executing it, so there is
// effectively no stream-establishment hook.
//
// kratos's own stream interceptor runs before this one, so ss.Context() here already carries the
// Transport and auth can read request headers directly.
func streamChain(chain *mw.Chain) grpc.StreamServerInterceptor {
	ms := chain.Build()
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		h := func(ctx context.Context, _ any) (any, error) {
			return nil, handler(srv, ctxStream{ss, ctx})
		}
		_, err := middleware.Chain(ms...)(h)(ss.Context(), nil)
		return err
	}
}

// ctxStream hands the context injected by the middleware chain (Claims, tracing span and so on) to the streaming
// handler; kratos's wrappedStream still uses its own context internally to send and receive messages.
type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s ctxStream) Context() context.Context { return s.ctx }

// StreamRegister is called after proto handlers are registered. Use sc to
// register SSE/download/log-stream handlers on the same HTTP server.
type StreamRegister func(sc *StreamContext)

// StreamContext gives raw routes (http.Handlers that bypass the middleware chain) the server handle and the
// authentication entries. There are two registration entries to choose from, RequireLogin() or Anonymous() (see
// stream.go), and no way to stay undeclared: forgetting the declaration means no method can be picked at compile
// time, rather than silently allowing anonymous access at run time.
type StreamContext struct {
	server *HTTPServer
	// requireLogin / anonymous being nil means the server has no authentication; then RequireLogin() records a
	// construction-time error (login declared but nothing to authenticate with), and Anonymous() degrades to pass-through.
	requireLogin func(http.Handler) http.Handler
	anonymous    func(http.Handler) http.Handler
	// err accumulates errors from RequireLogin() being called without authentication config; NewHTTPServer returns
	// it once after running all streamRegister callbacks, the same rule as mw.Chain.Err(): startup errors have only
	// one exit.
	err error
	// filters are the pre-routing filters mounted by Filter(), nested from outside in, in mount order.
	filters []func(http.Handler) http.Handler
}
