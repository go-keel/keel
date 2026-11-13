package metadata

import (
	"context"
	"strings"

	"github.com/go-kratos/kratos/v3/metadata"
	"github.com/go-kratos/kratos/v3/middleware"
	mmd "github.com/go-kratos/kratos/v3/middleware/metadata"
	"github.com/go-kratos/kratos/v3/transport"
	"github.com/go-kratos/kratos/v3/transport/http"
	"google.golang.org/grpc/peer"

	"github.com/go-keel/keel/internal/clientip"
)

const KeyPrefix = "x-keel-"

const KeyAcceptLanguage = KeyPrefix + "accept-language"
const KeyRemoteAddress = KeyPrefix + "remote-address"

type Metadata metadata.Metadata

func (m Metadata) AcceptLanguage() string {
	return metadata.Metadata(m).Get(KeyAcceptLanguage)
}

func (m Metadata) RemoteAddress() string {
	return metadata.Metadata(m).Get(KeyRemoteAddress)
}

func (m Metadata) Get(key string) string {
	return metadata.Metadata(m).Get(key)
}

// ServerOption configures the Server middleware.
type ServerOption func(*serverOptions)

type serverOptions struct {
	trustIncoming bool
}

// WithTrustIncoming trusts the incoming x-keel-remote-address and keeps the upstream value, so the client IP is
// passed along the whole call chain.
//
// By default no server (HTTP or gRPC) trusts the incoming value: the value computed at this hop always wins (trusting
// only the connection peer and the X-Forwarded-For entries appended by listed proxies), so outsiders cannot forge
// GetRemoteIP() via an X-Keel-Remote-Address header.
// Only services that explicitly accept internal traffic only should enable this option: internal gRPC services, or
// internal HTTP services behind a trusted gateway.
//
// Any server that may receive external traffic directly must not enable it: once enabled, an
// X-Keel-Remote-Address brought by the requester is taken as is.
func WithTrustIncoming() ServerOption {
	return func(o *serverOptions) { o.trustIncoming = true }
}

// Server creates a middleware that handles server-side metadata.
// It propagates custom metadata along the whole call chain,
// mainly to support i18n, obtaining the client address, and passing any x-keel- prefixed metadata.
func Server(opts ...ServerOption) middleware.Middleware {
	o := &serverOptions{}
	for _, opt := range opts {
		opt(o)
	}

	// The leading metadata middleware, which handles incoming metadata
	metadataMiddleware := mmd.Server(
		mmd.WithPropagatedPrefix(KeyPrefix),
	)

	currentMiddleware := func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (resp any, err error) {
			md, ok := metadata.FromServerContext(ctx)
			if !ok {
				return handler(ctx, req)
			}

			tr, ok := transport.FromServerContext(ctx)
			if !ok {
				// A normal call path always has a transport; do not panic, skip the enrichment and continue.
				return handler(ctx, req)
			}

			// If the context has no accept-language, take it from the request
			// (Accept-Language is freely set by the client anyway; there is no trust issue)
			if md.Get(KeyAcceptLanguage) == "" {
				md.Set(KeyAcceptLanguage, tr.RequestHeader().Get("Accept-Language"))
			}

			setRemoteAddress(ctx, md, tr, o.trustIncoming)
			return handler(ctx, req)
		}
	}

	return middleware.Chain(metadataMiddleware, currentMiddleware)
}

// setRemoteAddress maintains a framework invariant: x-keel-remote-address in ctx must be trustworthy.
//
// By default the incoming x-keel-remote-address is not read: it is always overwritten with the value computed at
// this hop, and when nothing can be computed the key is deleted, leaving it empty rather than a forged value. Only
// internal services with an explicit WithTrustIncoming keep the upstream value for end-to-end propagation, and they
// also fall back to computing it when upstream sent none.
//
// Clearing must use delete: kratos's Metadata.Set returns early on an empty value and cannot overwrite.
func setRemoteAddress(ctx context.Context, md metadata.Metadata, tr transport.Transporter, trustIncoming bool) {
	if trustIncoming && md.Get(KeyRemoteAddress) != "" {
		return
	}
	if ip := getClientIP(ctx, tr); ip != "" {
		md.Set(KeyRemoteAddress, ip)
		return
	}
	delete(md, strings.ToLower(KeyRemoteAddress))
}

func FromServerContext(ctx context.Context) (Metadata, bool) {
	md, ok := metadata.FromServerContext(ctx)
	if !ok {
		return nil, false
	}
	return Metadata(md), true
}

func GetAcceptLanguage(ctx context.Context) string {
	md, ok := FromServerContext(ctx)
	if !ok {
		return ""
	}
	return md.AcceptLanguage()
}

// GetRemoteIP returns the client IP. Inside the middleware chain it reads x-keel-remote-address; outside the chain
// (raw routes, filters mounted via StreamContext.Filter, passing r.Context()) it reads the value api.NewHTTPServer
// computed before routing with this port's client_ip config. Outside the chain there is no incoming metadata, so
// WithTrustIncoming has no effect and the value is always the one computed at this hop.
//
// When neither is present it returns an empty string rather than computing with the default rule: the caller's port
// may have a trusted list configured, and an address computed with the default rule would silently ignore it.
func GetRemoteIP(ctx context.Context) string {
	if md, ok := FromServerContext(ctx); ok {
		return md.RemoteAddress()
	}
	ip, _ := clientip.FromContext(ctx)
	return ip
}

func Client() middleware.Middleware {
	return mmd.Client(
		mmd.WithPropagatedPrefix(KeyPrefix),
	)
}

// getClientIP computes the client IP from the request actually received at this hop: it trusts only the connection
// peer, plus the X-Forwarded-For entries appended by listed proxies when the peer is on the trusted list; the rule is
// in internal/clientip.
//
// Servers built by api.NewHTTPServer compute it before routing with their own client_ip config and put it in the
// context: the same chain is reused by several servers and cannot tell which port a request came from, so it is not
// computed from config here. Requests that did not pass through it (gRPC servers, kratos HTTP servers users build
// themselves) are computed with the default rule (trust only the local host): for gRPC the peer is the grpc peer and
// the forwarding header is x-forwarded-for from the incoming metadata.
func getClientIP(ctx context.Context, tr transport.Transporter) string {
	if ip, ok := clientip.FromContext(ctx); ok {
		return ip
	}
	if t, ok := tr.(http.Transporter); ok {
		return clientip.Default.Resolve(t.Request().RemoteAddr, tr.RequestHeader().Values(clientip.HeaderForwardedFor))
	}
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return clientip.Default.Resolve(p.Addr.String(), tr.RequestHeader().Values(clientip.HeaderForwardedFor))
	}
	return ""
}
