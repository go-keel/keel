package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	khttp "github.com/go-kratos/kratos/v3/transport/http"
	"github.com/gorilla/websocket"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/middleware/auth"
	"github.com/go-keel/keel/mw"
	"github.com/go-keel/keel/pkg/jwt"
	stream "github.com/go-keel/keel/server/api"
)

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed int
}

func (f *flushRecorder) Flush() { f.flushed++ }

func TestSSEWriter_WriteEvent(t *testing.T) {
	w := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	sse, err := stream.NewSSEWriter(w)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	if err := sse.WriteEvent(stream.SSEEvent{ID: "42", Event: "update", Data: "hello"}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	body := w.Body.String()
	for _, want := range []string{"id: 42\n", "event: update\n", "data: hello\n", "\n\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got %q", want, body)
		}
	}
	if w.flushed != 1 {
		t.Errorf("expected 1 flush, got %d", w.flushed)
	}
}

// The SSE spec requires a multi-line payload to prefix every line with "data: ", otherwise a line without a colon is
// dropped whole by the client as an unknown field and an empty line is dispatched early as an event boundary.
func TestSSEWriter_MultilineData(t *testing.T) {
	w := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	sse, err := stream.NewSSEWriter(w)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	if err := sse.WriteEvent(stream.SSEEvent{Data: "line one\nline two"}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	if got, want := w.Body.String(), "data: line one\ndata: line two\n\n"; got != want {
		t.Errorf("body = %q; want %q", got, want)
	}
}

// nonFlusher is an http.ResponseWriter that does NOT implement http.Flusher.
type nonFlusher struct{ h http.Header }

func (n *nonFlusher) Header() http.Header         { return n.h }
func (n *nonFlusher) Write(b []byte) (int, error) { return len(b), nil }
func (n *nonFlusher) WriteHeader(int)             {}

func TestNewSSEWriter_NoFlusher(t *testing.T) {
	if _, err := stream.NewSSEWriter(&nonFlusher{h: http.Header{}}); err == nil {
		t.Fatal("expected error for non-flusher ResponseWriter")
	}
}

func TestSSEHandler_CancelOnDisconnect(t *testing.T) {
	done := make(chan struct{})
	handler := stream.SSEHandler(func(ctx context.Context, w *stream.SSEWriter) error {
		<-ctx.Done()
		close(done)
		return nil
	})
	req := httptest.NewRequest("GET", "/stream", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	w := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	go handler(w, req)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not observe context cancellation")
	}
}

func TestServeContent_ContentDisposition(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/file", nil)
	stream.ServeContent(w, r, strings.NewReader("data"), stream.DownloadOptions{Filename: "report.pdf"})
	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "report.pdf") {
		t.Fatalf("unexpected Content-Disposition: %q", cd)
	}
}

func TestWebSocketHandler_Echo(t *testing.T) {
	srv := httptest.NewServer(stream.WebSocketHandler(func(ctx context.Context, conn *websocket.Conn) error {
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return err // client closed → isExpectedWSClose classifies it as normal
			}
			if err := conn.WriteMessage(mt, data); err != nil {
				return err
			}
		}
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	defer func() { _ = conn.Close() }()

	if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "ping" {
		t.Fatalf("echo = %q, want ping", data)
	}
}

func TestWebSocketHandler_NormalCloseOnReturn(t *testing.T) {
	srv := httptest.NewServer(stream.WebSocketHandler(func(ctx context.Context, conn *websocket.Conn) error {
		return nil // end normally right away
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	defer func() { _ = conn.Close() }()

	_, _, err = conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		t.Fatalf("expected normal closure, got %v", err)
	}
}

func TestWebSocketHandler_ReadLimit(t *testing.T) {
	srv := httptest.NewServer(stream.WebSocketHandler(func(ctx context.Context, conn *websocket.Conn) error {
		_, _, err := conn.ReadMessage()
		return err
	}, stream.WSOptions{ReadLimit: 4}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	defer func() { _ = conn.Close() }()

	if err := conn.WriteMessage(websocket.TextMessage, []byte("exceeds-limit")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The server closes with 1009 (message too big): the client's mistake, not an internal error.
	_, _, err = conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseMessageTooBig) {
		t.Fatalf("read after an oversized frame: %v, want close 1009", err)
	}
}

// Wiring regression: auth.cookie_name in the server config must reach StreamContext.RequireLogin.
// Requests sent directly by the browser (img/EventSource/WebSocket handshake/download links) carry only the cookie,
// not the Authorization header; with an httpOnly cookie session, failing to get the token there makes the whole class
// of stream extension points unusable.
func TestStreamContextAuth_CookieSession(t *testing.T) {
	const secret = "test-secret"
	srv, err := stream.NewHTTPServer(&conf.Server_HTTP{
		Addr: "127.0.0.1:0",
		Auth: &conf.Server_Auth{
			Enabled:    true,
			Algorithm:  "HS256",
			JwtSecret:  secret,
			CookieName: "sess",
		},
	}, func(s *khttp.Server) {}, mw.Default(nil), func(sc *stream.StreamContext) {
		sc.RequireLogin().Handle("/protected", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, _ := auth.FromContext(r.Context())
			sub, _ := claims["sub"].(string)
			_, _ = io.WriteString(w, sub)
		}))
	})
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
	url := "http://" + ep.Host + "/protected"

	signer, err := jwt.NewHS256Signer([]byte(secret))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	token, err := signer.Sign(jwt.NewClaims("u1", time.Minute))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	// A cookie-session request should pass authentication, with claims in the context
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.AddCookie(&http.Cookie{Name: "sess", Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET with cookie: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "u1" {
		t.Fatalf("cookie session should pass authentication, got status %d body %q", resp.StatusCode, body)
	}

	// Without a credential it should still be 401
	resp2, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET without credentials: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no credential should be 401, got %d", resp2.StatusCode)
	}
}
