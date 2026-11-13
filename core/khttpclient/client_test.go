package khttpclient_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/khttpclient"
	"github.com/go-keel/keel/mw"
	"github.com/go-keel/keel/server/api"
	kerrors "github.com/go-kratos/kratos/v3/errors"
	khttp "github.com/go-kratos/kratos/v3/transport/http"
	"github.com/go-resty/resty/v2"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// newTestClient wraps New's three-return-value boilerplate and registers cleanup with t.Cleanup. All test files share
// it; the otel path is covered separately below, with a temporarily replaced global TracerProvider.
func newTestClient(t *testing.T, endpoint string, opts ...khttpclient.Option) *resty.Client {
	t.Helper()
	cli, cleanup, err := khttpclient.New(endpoint, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	return cli
}

// newProtoEchoServer starts a server that supports HTTP/1.1 and h2c on the same port and echoes the request protocol
// version.
func newProtoEchoServer(t *testing.T) *httptest.Server {
	t.Helper()
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	}))
	srv.Config.Protocols = &protocols
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func TestNew_InvalidEndpoint(t *testing.T) {
	for _, ep := range []string{"", "not-a-url", "ftp://x", "http://"} {
		if _, _, err := khttpclient.New(ep); err == nil {
			t.Errorf("New(%q) expected error", ep)
		}
	}
}

func TestNew_H2CRequiresHTTPScheme(t *testing.T) {
	if _, _, err := khttpclient.New("https://example.com", khttpclient.WithH2C()); err == nil {
		t.Fatal("expected error for WithH2C on https endpoint")
	}
}

func TestNew_DefaultUsesHTTP1OnCleartext(t *testing.T) {
	srv := newProtoEchoServer(t)
	cli := newTestClient(t, srv.URL)
	resp, err := cli.R().Get("/")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resp.Body()); got != "HTTP/1.1" {
		t.Fatalf("proto = %q, want HTTP/1.1", got)
	}
}

// The limit must really reach resty (an assembly test, not a test of upstream truncation semantics): exceeding it is
// an error, and 0 restores no limit.
func TestResponseBodyLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// It must be text: zero bytes would be sniffed as octet-stream and hit rejectNonText, and this case tests
		// the limit, not the content type.
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(bytes.Repeat([]byte("x"), 100))
	}))
	t.Cleanup(srv.Close)

	cli := newTestClient(t, srv.URL, khttpclient.WithResponseBodyLimit(99))
	if _, err := cli.R().Get("/"); err == nil {
		t.Fatal("a 100-byte response body should fail under a 99-byte limit")
	}

	// There is no "limit off" setting: 0 and negative numbers are rejected at construction.
	for _, n := range []int{0, -1} {
		if _, _, err := khttpclient.New(srv.URL, khttpclient.WithResponseBodyLimit(n)); err == nil {
			t.Errorf("WithResponseBodyLimit(%d) should fail", n)
		}
	}
	// Setting a specific value that is large enough still works
	if _, err := newTestClient(t, srv.URL, khttpclient.WithResponseBodyLimit(200)).R().Get("/"); err != nil {
		t.Fatalf("a 100-byte response should not fail under a 200-byte limit: %v", err)
	}
}

func TestNew_H2C(t *testing.T) {
	srv := newProtoEchoServer(t)
	cli := newTestClient(t, srv.URL, khttpclient.WithH2C())
	resp, err := cli.R().Get("/")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resp.Body()); got != "HTTP/2.0" {
		t.Fatalf("proto = %q, want HTTP/2.0", got)
	}
}

// startKeelServer starts a real server with the framework's own NewHTTPServer (h2c on by default, and the kratos JSON
// codec is the Keel server's protojson-style output). /app returns conf.App and echoes the request protocol version in
// its Version field; /conflict returns a business error.
func startKeelServer(t *testing.T) string {
	t.Helper()
	srv, err := api.NewHTTPServer(&conf.Server_HTTP{Addr: "127.0.0.1:0"}, func(s *khttp.Server) {
		s.Route("/").GET("/app", func(ctx khttp.Context) error {
			khttp.SetOperation(ctx, "/test/GetApp")
			h := ctx.Middleware(func(context.Context, any) (any, error) {
				return &conf.App{Name: "keel", Version: ctx.Request().Proto}, nil
			})
			out, err := h(ctx, &struct{}{})
			if err != nil {
				return err
			}
			return ctx.Result(200, out)
		})
		s.Route("/").GET("/conflict", func(ctx khttp.Context) error {
			khttp.SetOperation(ctx, "/test/Conflict")
			h := ctx.Middleware(func(context.Context, any) (any, error) {
				return nil, kerrors.New(409, "TODO_CONFLICT", "already exists")
			})
			_, err := h(ctx, &struct{}{})
			return err
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
	return "http://" + ep.Host
}

// appDTO models the response from the caller's point of view: a struct hand-written from the HTTP+JSON contract
// alone, without importing the upstream proto types. Its fields demonstrate the protojson-style wire shape: camelCase
// (nodeId), int64 encoded as a string, and unset fields present too (EmitUnpopulated).
type appDTO struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	NodeID  string `json:"nodeId"`
}

func TestIntegration_JSONContractOverH2C(t *testing.T) {
	base := startKeelServer(t)
	cli := newTestClient(t, base, khttpclient.WithH2C())

	var out appDTO
	if _, err := cli.R().SetContext(context.Background()).SetResult(&out).Get("/app"); err != nil {
		t.Fatal(err)
	}
	if out.Name != "keel" {
		t.Fatalf("name = %q, want keel", out.Name)
	}
	if out.Version != "HTTP/2.0" {
		t.Fatalf("server saw proto %q, want HTTP/2.0", out.Version)
	}
	// EmitUnpopulated + int64-as-string: the unset node_id appears as the string "0"
	if out.NodeID != "0" {
		t.Fatalf(`nodeId = %q, want "0"`, out.NodeID)
	}
}

func TestIntegration_KratosErrorEnvelope(t *testing.T) {
	base := startKeelServer(t)
	cli := newTestClient(t, base)

	_, err := cli.R().SetContext(context.Background()).Get("/conflict")
	// From the caller's point of view: check the reason string through the built-in helpers, without touching kratos
	// errors
	if !khttpclient.IsReason(err, "TODO_CONFLICT") {
		t.Fatalf("IsReason miss: %v", err)
	}
	info, ok := khttpclient.AsError(err)
	if !ok || info.Code != 409 {
		t.Fatalf("unexpected error info: %+v ok=%v", info, ok)
	}
}

// The path where the caller imports the other side's pb package: the Keel server's output decodes straight back into
// the generated type.
func TestIntegration_ProtoResultRoundTrip(t *testing.T) {
	base := startKeelServer(t)
	cli := newTestClient(t, base)

	var out conf.App
	if _, err := cli.R().SetContext(context.Background()).SetResult(khttpclient.ProtoResult(&out)).Get("/app"); err != nil {
		t.Fatal(err)
	}
	if out.GetName() != "keel" || out.GetVersion() != "HTTP/1.1" {
		t.Fatalf("unexpected message: %v", &out)
	}
}

// New takes no observability parameters, and when no provider is given explicitly otelhttp uses otel's global
// TracerProvider; so this test temporarily swaps the global for a real SDK provider to assert spans/attributes, and
// must restore it afterwards so it does not pollute other tests in the same process.
func TestNew_OTELTraceAndPropagation(t *testing.T) {
	var traceparent atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceparent.Store(r.Header.Get("traceparent"))
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	oldTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(oldTP) })

	cli, cleanup, err := khttpclient.New(srv.URL, khttpclient.WithServiceName("b-svc"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if _, err := cli.R().SetContext(context.Background()).Get("/"); err != nil {
		t.Fatal(err)
	}

	if got, _ := traceparent.Load().(string); got == "" {
		t.Error("traceparent header not propagated to server")
	}
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	found := false
	for _, attr := range spans[0].Attributes() {
		if string(attr.Key) == "peer.service" && attr.Value.AsString() == "b-svc" {
			found = true
		}
	}
	if !found {
		t.Errorf("span missing peer.service=b-svc, attrs: %v", spans[0].Attributes())
	}
}

// When nobody calls kotel.Setup (or otel.enabled=false), otel's global TracerProvider is the default no-op
// implementation: the span from Start has an invalid span context, so the TraceContext propagator refuses to inject
// traceparent (spec behaviour, not something khttpclient blocks itself). This is why "the globals are either fully
// configured or entirely no-op" holds without any nil-check logic.
func TestNew_NoGlobalProviderSkipsPropagation(t *testing.T) {
	var traceparent atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceparent.Store(r.Header.Get("traceparent"))
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	cli := newTestClient(t, srv.URL)
	if _, err := cli.R().Get("/"); err != nil {
		t.Fatal(err)
	}
	if got, _ := traceparent.Load().(string); got != "" {
		t.Errorf("the default global (no provider) should not inject traceparent, got %q", got)
	}
}
