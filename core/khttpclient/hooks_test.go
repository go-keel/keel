package khttpclient_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-keel/keel/core/khttpclient"
	kerrors "github.com/go-kratos/kratos/v3/errors"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// newEnvelopeServer starts a test server: /ok returns 200; /conflict returns a kratos error envelope with the fixed
// fields; /broken returns a 503 text response that is not an envelope.
func newEnvelopeServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/conflict", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		// The fixed envelope, consistent with the Keel server's error encoding
		_, _ = w.Write([]byte(`{"code":409,"reason":"TODO_CONFLICT","message":"already exists","metadata":{"id":"1"}}`))
	})
	mux.HandleFunc("/broken", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("oops, not a kratos envelope"))
	})
	// 3xx: not 2xx, yet resty's IsError() does not count it (it only counts >=400); used to verify that the log level
	// follows the same judgement as decodeError. The http client does not follow a 304 as a redirect.
	mux.HandleFunc("/notmodified", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	// 200 + binary: this client accepts only JSON/text, so such a response should be a call error.
	mux.HandleFunc("/binary", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{0x89, 0x50, 0x4e, 0x47})
	})
	// 200 + text/plain: plain text such as health checks must get through. Also assert that Accept advertises text;
	// advertising only application/json would make a peer that does content negotiation answer 406.
	mux.HandleFunc("/plain", func(w http.ResponseWriter, r *http.Request) {
		if accept := r.Header.Get("Accept"); !strings.Contains(accept, "text/") {
			t.Errorf("Accept does not declare that text is accepted: %q", accept)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	// 204: no response body, so it must not be stopped by the content-type check for "having no Content-Type".
	mux.HandleFunc("/nocontent", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDecodeError_KratosEnvelope(t *testing.T) {
	srv := newEnvelopeServer(t)
	cli := newTestClient(t, srv.URL)

	_, err := cli.R().Get("/conflict")
	var ke *kerrors.Error
	if !errors.As(err, &ke) {
		t.Fatalf("want *errors.Error, got %T: %v", err, err)
	}
	// The caller checks the reason string, without referencing the upstream enum
	if ke.Reason != "TODO_CONFLICT" || ke.Code != 409 || ke.Message != "already exists" {
		t.Fatalf("unexpected error: %+v", ke)
	}
	if ke.Metadata["id"] != "1" {
		t.Fatalf("metadata not restored: %+v", ke.Metadata)
	}
}

func TestDecodeError_NonEnvelopeFallback(t *testing.T) {
	srv := newEnvelopeServer(t)
	cli := newTestClient(t, srv.URL)

	_, err := cli.R().Get("/broken")
	var ke *kerrors.Error
	if !errors.As(err, &ke) {
		t.Fatalf("want *errors.Error, got %T: %v", err, err)
	}
	if ke.Code != 503 || ke.Reason != "" {
		t.Fatalf("unexpected fallback error: %+v", ke)
	}
}

// The path where a 2xx produces no call error is covered by TestLog_SuccessRequest as well (it also does GET /ok and
// asserts no error), so it has no case of its own.

// countLogs counts the captured log entries, for asserting the "exactly one per request" invariant.
func countLogs(out string) int { return strings.Count(out, `msg="http client"`) }

// captureLogs temporarily replaces slog's default output with an in-memory buffer and restores it when the test ends.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

func TestLog_SuccessRequest(t *testing.T) {
	srv := newEnvelopeServer(t)
	buf := captureLogs(t)
	cli := newTestClient(t, srv.URL, khttpclient.WithServiceName("b-svc"))

	if _, err := cli.R().Get("/ok"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"http client", "service=b-svc", "method=GET", "status=200"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q in: %s", want, out)
		}
	}
}

func TestLog_BusinessErrorLoggedOnce(t *testing.T) {
	srv := newEnvelopeServer(t)
	buf := captureLogs(t)
	cli := newTestClient(t, srv.URL)

	_, _ = cli.R().Get("/conflict")
	out := buf.String()
	if got := countLogs(out); got != 1 {
		t.Fatalf("expected exactly 1 log entry, got %d in: %s", got, out)
	}
	if !strings.Contains(out, "status=409") || !strings.Contains(out, "level=WARN") {
		t.Errorf("business error should log WARN with status: %s", out)
	}
}

func TestLog_TransportError(t *testing.T) {
	buf := captureLogs(t)
	// Unreachable port: transport failure, no response
	cli := newTestClient(t, "http://127.0.0.1:1", khttpclient.WithTimeout(500*time.Millisecond))

	if _, err := cli.R().Get("/x"); err == nil {
		t.Fatal("expected transport error")
	}
	out := buf.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "error=") {
		t.Errorf("transport error should log ERROR with error field: %s", out)
	}
}

// When reading the body fails (triggered here by the response body limit), resty returns before the response
// middleware and logResponse never runs; the request must still leave exactly one ERROR log entry.
func TestLog_BodyReadFailureStillLogsOnce(t *testing.T) {
	srv := newEnvelopeServer(t)
	buf := captureLogs(t)
	cli := newTestClient(t, srv.URL)

	if _, err := cli.R().SetResponseBodyLimit(1).Get("/conflict"); err == nil {
		t.Fatal("expected body limit error")
	}
	out := buf.String()
	if got := countLogs(out); got != 1 {
		t.Fatalf("expected exactly 1 log entry, got %d: %s", got, out)
	}
	if !strings.Contains(out, "level=ERROR") {
		t.Fatalf("a body read failure should log ERROR: %s", out)
	}
}

// JSON/text only: a 2xx binary response is a call error, since this client is not a download channel; under the 2MB
// limit a large file is cut off midway and a small one goes whole into memory, and neither is an "API call". The
// rejection must log exactly one ERROR through OnError, not first leave a status=200 INFO.
func TestCheckResponseType_BinaryRejected(t *testing.T) {
	srv := newEnvelopeServer(t)
	buf := captureLogs(t)
	cli := newTestClient(t, srv.URL)

	_, err := cli.R().Get("/binary")
	if err == nil {
		t.Fatal("a binary response should be a call error")
	}
	// Report the 200 the other side actually returned, not a disguised 500
	if info, _ := khttpclient.AsError(err); info.Code != http.StatusOK {
		t.Errorf("AsError Code = %d, want 200", info.Code)
	}
	out := buf.String()
	if got := countLogs(out); got != 1 {
		t.Fatalf("expected exactly 1 log entry, got %d: %s", got, out)
	}
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "status=200") {
		t.Errorf("a rejected response should log ERROR with status, not INFO: %s", out)
	}
}

// The three edges on the accepting side: plain text, a 204 with no body, and a non-2xx non-text error page; the last
// goes to decodeError to fall back to the status code and is not subject to the content-type check.
func TestCheckResponseType_Passthrough(t *testing.T) {
	srv := newEnvelopeServer(t)
	cli := newTestClient(t, srv.URL)

	resp, err := cli.R().Get("/plain")
	if err != nil {
		t.Errorf("text/plain should get through: %v", err)
	}
	// The raw string is the same path for JSON and text: resp.String(), not SetResult.
	if got := resp.String(); got != "ok" {
		t.Errorf("resp.String() = %q, want ok", got)
	}
	if _, err := cli.R().Get("/nocontent"); err != nil {
		t.Errorf("a 204 with no body should get through: %v", err)
	}
	// /broken is a 503 + plain text that is not an envelope; TestDecodeError_NonEnvelopeFallback already asserts the
	// fallback to the status code, and here we only confirm the content-type check did not stop it.
	_, err = cli.R().Get("/broken")
	var ke *kerrors.Error
	if !errors.As(err, &ke) || ke.Code != 503 {
		t.Errorf("a non-2xx should go through decodeError, not the content-type check: %v", err)
	}
}

// SetResult on a text response: resty decodes only JSON/XML, so that struct is never filled, and silently leaving the
// zero value is the hardest failure to track down. It must fail right away and point to resp.String().
func TestCheckResponseType_TextWithSetResultRejected(t *testing.T) {
	srv := newEnvelopeServer(t)
	buf := captureLogs(t)
	cli := newTestClient(t, srv.URL)

	var out struct {
		Status string `json:"status"`
	}
	_, err := cli.R().SetResult(&out).Get("/plain")
	if err == nil {
		t.Fatal("text response + SetResult should fail instead of silently leaving the zero value")
	}
	if !strings.Contains(err.Error(), "resp.String()") {
		t.Errorf("the error should point to the right way to read it: %v", err)
	}
	if logOut := buf.String(); countLogs(logOut) != 1 || !strings.Contains(logOut, "level=ERROR") {
		t.Errorf("want exactly one ERROR log entry: %s", logOut)
	}
	// SetResult on a JSON response is unaffected
	var ok struct{}
	if _, err := cli.R().SetResult(&ok).Get("/ok"); err != nil {
		t.Errorf("JSON + SetResult should work: %v", err)
	}
}

// A 3xx is restored into a call error by decodeError, so the log level must follow as WARN, not INFO just because
// resty's IsError() only counts >=400.
func TestLog_NonSuccess3xxLogsWarn(t *testing.T) {
	srv := newEnvelopeServer(t)
	buf := captureLogs(t)
	cli := newTestClient(t, srv.URL)

	if _, err := cli.R().Get("/notmodified"); err == nil {
		t.Fatal("a 304 should be returned as a call error")
	}
	out := buf.String()
	if got := countLogs(out); got != 1 {
		t.Fatalf("expected exactly 1 log entry, got %d: %s", got, out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "status=304") {
		t.Fatalf("a 3xx should log WARN with status: %s", out)
	}
}

// The ProtoResult path: the Keel server's protojson shape (int64 quoted, Timestamp as RFC3339, enums by name) must
// decode into the generated type, and new fields added by the other side must be ignored. The messages borrow
// well-known types rather than defining a proto for the test.
func TestProtoResult_ProtojsonWireShape(t *testing.T) {
	mux := http.NewServeMux()
	serve := func(path, body string) {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
	}
	// Above 2^53: going through float64 would lose the last digits
	serve("/int64", `{"negativeIntValue":"-9007199254740993"}`)
	serve("/timestamp", `"2026-10-27T08:00:00.5Z"`)
	serve("/enum", `{"name":"id","type":"TYPE_INT64","addedLater":{"x":1}}`)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cli := newTestClient(t, srv.URL)

	var opt descriptorpb.UninterpretedOption
	if _, err := cli.R().SetResult(khttpclient.ProtoResult(&opt)).Get("/int64"); err != nil {
		t.Fatalf("int64: %v", err)
	}
	if opt.GetNegativeIntValue() != -9007199254740993 {
		t.Errorf("int64 = %d", opt.GetNegativeIntValue())
	}

	var ts timestamppb.Timestamp
	if _, err := cli.R().SetResult(khttpclient.ProtoResult(&ts)).Get("/timestamp"); err != nil {
		t.Fatalf("timestamp: %v", err)
	}
	if want := time.Date(2026, 10, 27, 8, 0, 0, 5e8, time.UTC); !ts.AsTime().Equal(want) {
		t.Errorf("timestamp = %v, want %v", ts.AsTime(), want)
	}

	// Unknown field addedLater: an additive upgrade on the other side, and the caller should not fail first
	var fd descriptorpb.FieldDescriptorProto
	if _, err := cli.R().SetResult(khttpclient.ProtoResult(&fd)).Get("/enum"); err != nil {
		t.Fatalf("enum/unknown: %v", err)
	}
	if fd.GetType() != descriptorpb.FieldDescriptorProto_TYPE_INT64 || fd.GetName() != "id" {
		t.Errorf("unexpected message: %v", &fd)
	}
}

// A nil target fails right away, so protojson does not panic and the response is not silently dropped.
func TestProtoResult_NilTarget(t *testing.T) {
	srv := newEnvelopeServer(t)
	cli := newTestClient(t, srv.URL)

	var typedNil *timestamppb.Timestamp
	for name, msg := range map[string]proto.Message{"nil": nil, "typed nil": typedNil} {
		_, err := cli.R().SetResult(khttpclient.ProtoResult(msg)).Get("/ok")
		if err == nil || !strings.Contains(err.Error(), "ProtoResult") {
			t.Errorf("%s: want ProtoResult error, got %v", name, err)
		}
		// The caller's own programming error must not read as "the other side's 200 broke the contract"
		if info, _ := khttpclient.AsError(err); info.Code == http.StatusOK {
			t.Errorf("%s: a nil target should not be reported as a 2xx contract error: %+v", name, info)
		}
	}
}

// A 2xx decoding failure must be reported truthfully: AsError's Code is the 2xx the other side actually returned, not
// a disguised 500, and the log gets exactly one ERROR with status through OnError. Plain structs and ProtoResult take
// the same path. Passed out of the caller's own server it must not carry the 2xx, so it is not an *errors.Error.
func TestDecodeResult_FailureReportsActualStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/bad", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		// Neither target can decode it: the struct's label is a string, and the proto's name is a string
		_, _ = w.Write([]byte(`{"name":1,"label":1}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cli := newTestClient(t, srv.URL)

	var plain struct {
		Label string `json:"label"`
	}
	var fd descriptorpb.FieldDescriptorProto
	for name, target := range map[string]any{"SetResult": &plain, "ProtoResult": khttpclient.ProtoResult(&fd)} {
		buf := captureLogs(t)
		_, err := cli.R().SetResult(target).Get("/bad")
		info, ok := khttpclient.AsError(err)
		if !ok || info.Code != http.StatusCreated || info.Reason != "" || !strings.Contains(info.Message, "failed to decode") {
			t.Errorf("%s: AsError = %+v ok=%v, want Code=201 and the decoding failure reason", name, info, ok)
		}
		var ke *kerrors.Error
		if errors.As(err, &ke) {
			t.Errorf("%s: should not be an *errors.Error (passing it through would write back the 2xx): %v", name, ke)
		}
		if out := buf.String(); countLogs(out) != 1 || !strings.Contains(out, "level=ERROR") ||
			!strings.Contains(out, "status=201") {
			t.Errorf("%s: want exactly one ERROR with status: %s", name, out)
		}
	}
}

// A request with ProtoResult that gets a non-2xx leaves the target alone, and the error envelope is restored as usual.
func TestProtoResult_ErrorEnvelopeRestored(t *testing.T) {
	srv := newEnvelopeServer(t)
	cli := newTestClient(t, srv.URL)

	var ts timestamppb.Timestamp
	_, err := cli.R().SetResult(khttpclient.ProtoResult(&ts)).Get("/conflict")
	if !khttpclient.IsReason(err, "TODO_CONFLICT") {
		t.Fatalf("envelope not restored: %v", err)
	}
}

// With resty's JSONUnmarshal replaced, the SetError target is still filled on a best-effort basis, as resty does.
func TestDecodeError_SetErrorStillFilled(t *testing.T) {
	srv := newEnvelopeServer(t)
	cli := newTestClient(t, srv.URL)

	var env struct {
		Reason string `json:"reason"`
	}
	_, _ = cli.R().SetError(&env).Get("/conflict")
	if env.Reason != "TODO_CONFLICT" {
		t.Fatalf("SetError target not filled: %+v", env)
	}
}
