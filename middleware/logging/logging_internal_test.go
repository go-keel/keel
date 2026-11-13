// In-package: checks the formatting helpers the middleware uses internally, through a fake transport.

package logging

import (
	"bytes"
	"context"
	stderrors "errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/go-keel/keel/conf"
	"github.com/go-kratos/kratos/v3/encoding"
	_ "github.com/go-kratos/kratos/v3/encoding/json"
	"github.com/go-kratos/kratos/v3/errors"
	"github.com/go-kratos/kratos/v3/transport"
)

func TestSizeLimited_UnderLimit(t *testing.T) {
	if got := sizeLimited([]byte("hello"), 100); got != "hello" {
		t.Errorf("got %q", got)
	}
}

func TestSizeLimited_TruncatesKeepingPrefix(t *testing.T) {
	data := []byte(strings.Repeat("a", 100))
	got := sizeLimited(data, 10)
	if !strings.HasPrefix(got, "aaaaaaaaaa") || !strings.HasSuffix(got, "...[truncated]") {
		t.Errorf("want 10-byte prefix + truncated marker, got %q", got)
	}
}

func TestSizeLimited_UTF8Boundary(t *testing.T) {
	// Chinese takes 3 bytes per character; a 4-byte limit must not cut a character in half.
	got := sizeLimited([]byte("中文日志"), 4)
	prefix := strings.TrimSuffix(got, "...[truncated]")
	if prefix != "中" {
		t.Errorf("want UTF-8 safe prefix %q, got %q", "中", prefix)
	}
}

func TestSizeLimited_NoLimit(t *testing.T) {
	data := []byte(strings.Repeat("x", 9999))
	if got := sizeLimited(data, 0); len(got) != 9999 {
		t.Errorf("maxSize=0 should not limit, got len %d", len(got))
	}
}

func TestMarshalBody_Binary(t *testing.T) {
	if got := marshalBody([]byte{1, 2, 3}, 100); got != "[binary]" {
		t.Errorf("got %q", got)
	}
}

func TestMarshalBody_Nil(t *testing.T) {
	if got := marshalBody(nil, 100); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestMarshalBody_AlignsWithWireCodec(t *testing.T) {
	// The log body must match the Keel HTTP default encoding: proto → "protojson" codec.
	msg := &conf.App{Name: "svc"}
	want, err := encoding.GetCodec("protojson").Marshal(msg)
	if err != nil {
		t.Fatalf("codec marshal: %v", err)
	}
	if got := marshalBody(msg, 0); got != string(want) {
		t.Errorf("log body %q != encoded body %q", got, want)
	}
	// Non-proto values use the "json" codec.
	plainWant, _ := encoding.GetCodec("json").Marshal(map[string]int{"a": 1})
	if got := marshalBody(map[string]int{"a": 1}, 0); got != string(plainWant) {
		t.Errorf("plain log body %q != encoded body %q", got, plainWant)
	}
}

func TestMarshalBody_PlainStruct(t *testing.T) {
	got := marshalBody(struct {
		Name string `json:"name"`
	}{Name: "k"}, 0)
	if !strings.Contains(got, `"name"`) {
		t.Errorf("json output expected, got %q", got)
	}
}

func TestSensitiveHeaderRedaction(t *testing.T) {
	// Verify the redaction list matching directly (case-insensitive).
	for _, h := range []string{"Authorization", "COOKIE", "x-api-key", "Proxy-Authorization", "Set-Cookie"} {
		if _, ok := sensitiveHeaders[strings.ToLower(h)]; !ok {
			t.Errorf("header %q should be in sensitive list", h)
		}
	}
	if _, ok := sensitiveHeaders["content-type"]; ok {
		t.Error("content-type should not be sensitive")
	}
}

func TestFormatHeaders(t *testing.T) {
	got := formatHeaders(map[string]string{"A": "1"})
	if got != "A:1" {
		t.Errorf("got %q", got)
	}
	if formatHeaders(nil) != "" {
		t.Error("empty headers should format to empty string")
	}
}

// Failures caused by the caller (4xx) log at WARN; service faults and unrecognised errors log at ERROR; status codes unchanged.
func TestExtractError_LevelByStatus(t *testing.T) {
	cases := []struct {
		name      string
		tr        *miniTransport
		err       error
		wantLevel slog.Level
		wantCode  int32
	}{
		{name: "http success", tr: &miniTransport{}, wantLevel: slog.LevelInfo, wantCode: 200},
		{name: "non-http success", wantLevel: slog.LevelInfo, wantCode: 0},
		{name: "401", tr: &miniTransport{}, err: errors.Unauthorized("UNAUTHORIZED", "x"), wantLevel: slog.LevelWarn, wantCode: 401},
		{name: "404", tr: &miniTransport{}, err: errors.NotFound("NOT_FOUND", "x"), wantLevel: slog.LevelWarn, wantCode: 404},
		{name: "499 client closed", tr: &miniTransport{}, err: errors.New(499, "CLIENT_CLOSED", "x"), wantLevel: slog.LevelWarn, wantCode: 499},
		{name: "500", tr: &miniTransport{}, err: errors.InternalServer("INTERNAL", "x"), wantLevel: slog.LevelError, wantCode: 500},
		{name: "503", tr: &miniTransport{}, err: errors.ServiceUnavailable("UNAVAILABLE", "x"), wantLevel: slog.LevelError, wantCode: 503},
		{name: "plain error as 500", tr: &miniTransport{}, err: stderrors.New("boom"), wantLevel: slog.LevelError, wantCode: 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tr transport.Transporter
			if tc.tr != nil {
				tr = tc.tr
			}
			level, code := extractError(tr, tc.err)
			if level != tc.wantLevel || code != tc.wantCode {
				t.Fatalf("extractError = (%v, %d), want (%v, %d)", level, code, tc.wantLevel, tc.wantCode)
			}
		})
	}
}

// miniTransport is a minimal transport.Transporter (avoids starting a real khttp server).
type miniTransport struct {
	op     string
	header transport.Header
}

func (m *miniTransport) Kind() transport.Kind            { return transport.KindHTTP }
func (m *miniTransport) Endpoint() string                { return "" }
func (m *miniTransport) Operation() string               { return m.op }
func (m *miniTransport) RequestHeader() transport.Header { return m.header }
func (m *miniTransport) ReplyHeader() transport.Header   { return m.header }

type headerMap map[string][]string

func (h headerMap) Get(key string) string {
	v := h[key]
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
func (h headerMap) Set(key, value string) { h[key] = []string{value} }
func (h headerMap) Add(key, value string) { h[key] = append(h[key], value) }
func (h headerMap) Keys() []string {
	ks := make([]string, 0, len(h))
	for k := range h {
		ks = append(ks, k)
	}
	return ks
}
func (h headerMap) Values(key string) []string { return h[key] }

func TestServer_StreamOpenLog(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	mw := Server()
	h := mw(func(ctx context.Context, req any) (any, error) { return nil, nil })

	// WebSocket signature
	ctx := transport.NewServerContext(context.Background(), &miniTransport{
		op:     "/ws",
		header: headerMap{"Upgrade": {"websocket"}},
	})
	if _, err := h(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "stream=websocket") || !strings.Contains(out, "phase=open") {
		t.Fatalf("missing websocket open log: %s", out)
	}

	// SSE signature
	buf.Reset()
	ctx = transport.NewServerContext(context.Background(), &miniTransport{
		op:     "/sse",
		header: headerMap{"Accept": {"text/event-stream"}},
	})
	if _, err := h(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "stream=sse") {
		t.Fatalf("missing sse open log: %s", out)
	}

	// A plain request has no open log
	buf.Reset()
	ctx = transport.NewServerContext(context.Background(), &miniTransport{
		op:     "/unary",
		header: headerMap{},
	})
	if _, err := h(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); strings.Contains(out, "phase=open") {
		t.Fatalf("unary should not emit open log: %s", out)
	}
}
