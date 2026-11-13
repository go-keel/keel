// Package clientip computes the client IP from "connection peer + trusted list". server/api assembles it per port,
// middleware/metadata writes it into x-keel-remote-address, and GetRemoteIP hands it to raw routes and pre-routing
// filters outside the middleware chain.
//
// It is internal because the computed address reaches the metadata middleware through the request context; if the
// write entry point were exported, users could bypass the trusted list and rewrite the client IP at will, which is
// exactly what this package guards.
package clientip

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/internal/errs"
)

// HeaderForwardedFor is the only forwarding header that is read. X-Real-IP and CDN-specific headers
// (CF-Connecting-IP and the like) are never read, for the reasons in ADR keel-0003; nor is RFC 7239 Forwarded.
const HeaderForwardedFor = "X-Forwarded-For"

// Resolver is one server's address rule. Its zero value is unusable; use New or Default.
type Resolver struct {
	trusted   []netip.Prefix
	ignoreXFF bool
}

// Default is the rule when no client_ip section is written: trust only the local host. gRPC servers take no config and always use it.
var Default = &Resolver{trusted: []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
}}

// New builds the rule from server.http.<name>.client_ip. It returns Default when cfg is nil or neither field is set;
// a written trusted_proxies replaces the default list entirely and is not merged with the local host. To trust the
// local host, write it in.
//
// The validity of each entry is checked by the rules on conf.proto when config loads; a parse failure here can only
// come from callers that build the config directly, bypassing kconfig, and it still errors rather than silently
// dropping the entry.
func New(cfg *conf.Server_ClientIP) (*Resolver, error) {
	if cfg.GetIgnoreForwardedFor() {
		return &Resolver{ignoreXFF: true}, nil
	}
	if len(cfg.GetTrustedProxies()) == 0 {
		return Default, nil
	}
	r := &Resolver{trusted: make([]netip.Prefix, 0, len(cfg.GetTrustedProxies()))}
	for _, s := range cfg.GetTrustedProxies() {
		p, err := parsePrefix(s)
		if err != nil {
			return nil, errs.Rules.New("api.trusted-proxy-invalid", fmt.Sprintf("client_ip.trusted_proxies has %q, which is not an address or CIDR range: %v", s, err), "write each entry as an IP address or a masked CIDR range without a zone, such as 10.0.0.0/8")
		}
		r.trusted = append(r.trusted, p)
	}
	return r, nil
}

// parsePrefix accepts a single address or a prefix. An IPv4-mapped IPv6 form is restored to IPv4; otherwise it would
// never match a peer address normalised with Unmap, and writing it would have no effect.
func parsePrefix(s string) (netip.Prefix, error) {
	if !strings.Contains(s, "/") {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		if a.Zone() != "" {
			return netip.Prefix{}, fmt.Errorf("zone not allowed")
		}
		a = a.Unmap()
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	if p != p.Masked() {
		return netip.Prefix{}, fmt.Errorf("host bits must be zero")
	}
	if a := p.Addr(); a.Is4In6() && p.Bits() >= 96 {
		return netip.PrefixFrom(a.Unmap(), p.Bits()-96), nil
	}
	return p, nil
}

func (r *Resolver) isTrusted(a netip.Addr) bool {
	for _, p := range r.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Resolve computes the client IP from the connection peer address (host:port or a bare address) and all lines of
// X-Forwarded-For; it returns an empty string when the peer cannot be parsed (for example a unix socket).
//
// When the peer is untrusted, the whole XFF may have been written by the client itself, so none of it can be read.
// When the peer is trusted, only the entries appended by it and by listed servers further right are trustworthy, so
// it searches right to left for the first entry not in the list: everything further left was brought by that client
// and can be forged. If that entry itself is invalid (unknown, empty, with a zone), the forwarding chain did not
// write the header as agreed, so it falls back to the peer instead of continuing left; going left only reaches
// values the client can write.
//
// xff must be all lines of the header (Header.Values), not just the first: when the client brings one line and a
// proxy appends another, the first line is exactly the forged entry.
func (r *Resolver) Resolve(peer string, xff []string) string {
	p, ok := parseAddr(peer)
	if !ok {
		return ""
	}
	if r.ignoreXFF || len(xff) == 0 || !r.isTrusted(p) {
		return p.String()
	}
	items := strings.Split(strings.Join(xff, ","), ",")
	for i := len(items) - 1; i >= 0; i-- {
		a, ok := parseAddr(items[i])
		if !ok {
			return p.String()
		}
		if !r.isTrusted(a) || i == 0 {
			// i == 0: the whole list is trusted, take the leftmost entry.
			return a.String()
		}
	}
	return p.String()
}

// parseAddr parses one address entry, with an optional port: 1.2.3.4, 1.2.3.4:5678, 2001:db8::1,
// [2001:db8::1], [2001:db8::1]:443. The result is normalised to netip's form with IPv4-mapped addresses restored to
// IPv4, so list matching and the return value use the same form. An address with a zone is meaningful only on the
// local link, is not a client IP, and is treated as invalid.
func parseAddr(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	a, err := netip.ParseAddr(s)
	if err != nil {
		if inner, ok := strings.CutPrefix(s, "["); ok && strings.HasSuffix(inner, "]") {
			a, err = netip.ParseAddr(strings.TrimSuffix(inner, "]"))
		} else {
			var ap netip.AddrPort
			ap, err = netip.ParseAddrPort(s)
			a = ap.Addr()
		}
	}
	if err != nil || a.Zone() != "" {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

type ctxKey struct{}

// Filter computes the client IP with this server's rule before the request is routed and puts it in the request
// context, for the metadata middleware and GetRemoteIP outside the chain. It sits at the server layer rather than in
// the middleware chain: the same base chain is reused by several servers, and middleware on it cannot tell which
// port a request came from.
func (r *Resolver) Filter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ip := r.Resolve(req.RemoteAddr, req.Header.Values(HeaderForwardedFor))
		next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), ctxKey{}, ip)))
	})
}

// FromContext returns the client IP computed by Filter; ok is false when the request did not pass through Filter
// (the server was not built by api.NewHTTPServer), and the caller computes it itself with the Default rule.
func FromContext(ctx context.Context) (ip string, ok bool) {
	ip, ok = ctx.Value(ctxKey{}).(string)
	return ip, ok
}
