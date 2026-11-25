package metadata_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-keel/keel/internal/clientip"
	"github.com/go-keel/keel/middleware/metadata"
	"github.com/go-keel/keel/pkg/rule"
	"github.com/go-kratos/kratos/v3/transport"
	khttp "github.com/go-kratos/kratos/v3/transport/http"
	"google.golang.org/grpc/peer"
)

type mockHeader map[string][]string

func (h mockHeader) Get(k string) string {
	if v := h[strings.ToLower(k)]; len(v) > 0 {
		return v[0]
	}
	return ""
}
func (h mockHeader) Set(k, v string)          { h[strings.ToLower(k)] = []string{v} }
func (h mockHeader) Add(k, v string)          { h[strings.ToLower(k)] = append(h[strings.ToLower(k)], v) }
func (h mockHeader) Values(k string) []string { return h[strings.ToLower(k)] }
func (h mockHeader) Keys() []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	return keys
}

type mockTransport struct {
	h    mockHeader
	kind transport.Kind
}

func (m *mockTransport) Kind() transport.Kind            { return m.kind }
func (m *mockTransport) Endpoint() string                { return "" }
func (m *mockTransport) Operation() string               { return "/test/Op" }
func (m *mockTransport) RequestHeader() transport.Header { return m.h }
func (m *mockTransport) ReplyHeader() transport.Header   { return mockHeader{} }

// mockHTTPTransport carries a real *http.Request and takes the http.Transporter branch (it did not pass through
// api.NewHTTPServer's Filter, so the default rule computes the address).
type mockHTTPTransport struct {
	mockTransport
	req *http.Request
}

func (m *mockHTTPTransport) Request() *http.Request { return m.req }
func (m *mockHTTPTransport) PathTemplate() string   { return "" }

var _ khttp.Transporter = (*mockHTTPTransport)(nil)

// remoteIPThrough passes a request through the Server middleware and returns the GetRemoteIP the business side sees.
func remoteIPThrough(t *testing.T, ctx context.Context, tr transport.Transporter, opts ...metadata.ServerOption) string {
	t.Helper()
	ctx = transport.NewServerContext(ctx, tr)
	var got string
	if _, err := metadata.Server(opts...)(func(ctx context.Context, req any) (any, error) {
		got = metadata.GetRemoteIP(ctx)
		return nil, nil
	})(ctx, nil); err != nil {
		t.Fatalf("middleware: %v", err)
	}
	return got
}

func grpcCtx(ip string) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP(ip), Port: 5000}})
}

func httpTr(t *testing.T, remoteAddr string, h mockHeader) *mockHTTPTransport {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.RemoteAddr = remoteAddr
	return &mockHTTPTransport{mockTransport: mockTransport{h: h, kind: transport.KindHTTP}, req: req}
}

// By default the incoming x-keel-remote-address is not trusted, for HTTP and gRPC alike; the value computed at this hop always wins.
func TestServer_IgnoresIncomingAddressByDefault(t *testing.T) {
	h := mockHeader{}
	h.Set(metadata.KeyRemoteAddress, "6.6.6.6") // forged value
	if ip := remoteIPThrough(t, grpcCtx("8.8.8.8"), &mockTransport{h: h, kind: transport.KindGRPC}); ip != "8.8.8.8" {
		t.Errorf("grpc: must recompute from peer, got %q", ip)
	}
	if ip := remoteIPThrough(t, context.Background(), httpTr(t, "8.8.8.8:1", h)); ip != "8.8.8.8" {
		t.Errorf("http: must recompute from RemoteAddr, got %q", ip)
	}
}

// When no transport can provide a peer, the incoming value must be cleared: empty is acceptable, forged is not.
func TestServer_ClearsUncomputableAddress(t *testing.T) {
	h := mockHeader{}
	h.Set(metadata.KeyRemoteAddress, "6.6.6.6")
	h.Set("X-Forwarded-For", "1.1.1.1")
	if ip := remoteIPThrough(t, context.Background(), &mockTransport{h: h, kind: transport.KindGRPC}); ip != "" {
		t.Errorf("forged address must not survive, got %q", ip)
	}
}

// X-Real-IP is no longer a source: even with a trusted peer, only X-Forwarded-For is read.
func TestServer_IgnoresXRealIP(t *testing.T) {
	h := mockHeader{}
	h.Set("X-Real-IP", "5.6.7.8")
	if ip := remoteIPThrough(t, context.Background(), httpTr(t, "127.0.0.1:1", h)); ip != "127.0.0.1" {
		t.Errorf("X-Real-IP must be ignored, got %q", ip)
	}
}

// gRPC takes no config and uses the default rule: incoming x-forwarded-for is read (all values, right to left) only when the peer is local.
func TestServer_GRPCDefaultRule(t *testing.T) {
	h := mockHeader{}
	h.Add("x-forwarded-for", "6.6.6.6")
	h.Add("x-forwarded-for", "1.1.1.1")
	if ip := remoteIPThrough(t, grpcCtx("127.0.0.1"), &mockTransport{h: h, kind: transport.KindGRPC}); ip != "1.1.1.1" {
		t.Errorf("loopback peer: got %q, want 1.1.1.1", ip)
	}
	if ip := remoteIPThrough(t, grpcCtx("8.8.8.8"), &mockTransport{h: h, kind: transport.KindGRPC}); ip != "8.8.8.8" {
		t.Errorf("untrusted peer: got %q, want 8.8.8.8", ip)
	}
}

// WithTrustIncoming: internal services keep the upstream value so the client IP propagates end to end.
func TestServer_TrustIncomingKeepsUpstream(t *testing.T) {
	h := mockHeader{}
	h.Set(metadata.KeyRemoteAddress, "1.2.3.4") // upstream value wins over the one computed at this hop
	if ip := remoteIPThrough(t, grpcCtx("10.0.0.1"), &mockTransport{h: h, kind: transport.KindGRPC}, metadata.WithTrustIncoming()); ip != "1.2.3.4" {
		t.Errorf("trusted mode should keep upstream value, got %q", ip)
	}
}

// In trust mode, when upstream sent nothing it still falls back to computing; if that fails it is empty, never forged.
func TestServer_TrustIncomingFallsBackToComputed(t *testing.T) {
	if ip := remoteIPThrough(t, grpcCtx("10.0.0.1"), &mockTransport{h: mockHeader{}, kind: transport.KindGRPC}, metadata.WithTrustIncoming()); ip != "10.0.0.1" {
		t.Errorf("trusted mode without upstream value should recompute, got %q", ip)
	}
	if ip := remoteIPThrough(t, context.Background(), &mockTransport{h: mockHeader{}, kind: transport.KindGRPC}, metadata.WithTrustIncoming()); ip != "" {
		t.Errorf("nothing to compute should yield empty, got %q", ip)
	}
}

// Accept-Language is freely set by the client, with no trust issue: when missing it is read from the request header.
func TestServer_AcceptLanguage(t *testing.T) {
	h := mockHeader{}
	h.Set("Accept-Language", "zh-CN")
	ctx := transport.NewServerContext(context.Background(), &mockTransport{h: h, kind: transport.KindHTTP})
	var got string
	if _, err := metadata.Server()(func(ctx context.Context, req any) (any, error) {
		got = metadata.GetAcceptLanguage(ctx)
		return nil, nil
	})(ctx, nil); err != nil {
		t.Fatalf("middleware: %v", err)
	}
	if got != "zh-CN" {
		t.Errorf("accept-language = %q, want zh-CN", got)
	}
}

// Outside the chain there is no x-keel-remote-address and no address step from api.NewHTTPServer: it returns an
// empty string rather than computing with the default rule.
func TestGetRemoteIP_NoSource(t *testing.T) {
	if got := metadata.GetRemoteIP(context.Background()); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// On a raw route the address comes from the step api.NewHTTPServer runs before routing. Set gives that ctx metadata of
// its own, and the address must still be readable afterwards.
func TestGetRemoteIP_RawRouteAfterSet(t *testing.T) {
	res, err := clientip.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var before, after string
	h := res.Filter(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		before = metadata.GetRemoteIP(r.Context())
		ctx, err := metadata.Set(r.Context(), "x-keel-notebook-id", "n1")
		if err != nil {
			t.Fatal(err)
		}
		after = metadata.GetRemoteIP(ctx)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.7:1234"
	h.ServeHTTP(httptest.NewRecorder(), req)
	if before != "203.0.113.7" || after != before {
		t.Fatalf("before Set %q, after Set %q, want 203.0.113.7 both times", before, after)
	}
}

// Set copies the request's metadata into a new ctx: the old ctx keeps its value, the new one has both keys.
func TestSet_CopiesMetadata(t *testing.T) {
	base, err := metadata.Set(context.Background(), "x-keel-notebook-id", "n1")
	if err != nil {
		t.Fatalf("Set on an empty ctx: %v", err)
	}
	next, err := metadata.Set(base, "X-Keel-Tenant", "t1")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := metadata.Get(next, "x-keel-notebook-id") + "|" + metadata.Get(next, "x-keel-tenant"); got != "n1|t1" {
		t.Errorf("new ctx = %q, want n1|t1", got)
	}
	if got := metadata.Get(base, "x-keel-tenant"); got != "" {
		t.Errorf("old ctx tenant = %q, want empty", got)
	}
}

// Only x-keel-* keys are passed between services, so Set refuses any other key.
func TestSet_RejectsKeyWithoutPrefix(t *testing.T) {
	_, err := metadata.Set(context.Background(), "tenant", "t1")
	if re, ok := errors.AsType[*rule.Error](err); !ok || re.ID != "metadata.key-prefix" {
		t.Fatalf("err = %v, want metadata.key-prefix", err)
	}
}
