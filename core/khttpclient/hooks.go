package khttpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	kerrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/go-resty/resty/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/go-keel/keel/internal/errs"
)

// ProtoResult makes a 2xx JSON response decode into msg with protojson; use it as
// cli.R().SetResult(khttpclient.ProtoResult(&out)). Use it when the caller imports the other side's pb package and
// wants the generated type: the HTTP endpoints of Keel services always emit protojson (int64 quoted, Timestamp as
// RFC3339, enums by name), and SetResult(&pb.Xxx{}) going straight through encoding/json fails to decode.
//
// DiscardUnknown is always on: additive upgrades on the other side (new fields) are normal, and the caller should
// not fail before the other side does; this matches the leniency of the Keel server when decoding request bodies.
//
// Read the result from msg itself; resp.Result() returns an internal wrapper. Decoding goes through decodeResult just
// like SetResult, and the content-type constraints and failure errors are unchanged (see checkResponseType). After a
// decoding failure msg may have been cleared or half written (protojson resets before writing), so do not use that
// target after an error.
func ProtoResult(msg proto.Message) any { return &protoResult{msg: msg} }

// protoResult plugs into decodeResult's json.Unmarshal through json.Unmarshaler, so SetResult's behaviour for plain
// structs stays exactly the same and the two paths are chosen only by the target the caller passes explicitly. It
// must be given to SetResult as a pointer: for a non-pointer value resty does reflect.New of a fresh zero value
// (getPointer in resty@v2.17.2 util.go), and msg would be lost.
type protoResult struct{ msg proto.Message }

// errNilProtoTarget is the caller's own programming error and has nothing to do with the other side: decodeResult
// does not wrap it in a contractError, or it would read as "the other side's 2xx broke the contract".
var errNilProtoTarget = errors.New("khttpclient: the ProtoResult target is nil; pass &pb.Xxx{}")

func (p *protoResult) UnmarshalJSON(b []byte) error {
	// nil and typed nil pointers fail right here: protojson would panic on them.
	if p.msg == nil || !p.msg.ProtoReflect().IsValid() {
		return errs.Rules.Wrap(errNilProtoTarget, "khttpclient.nil-result", fmt.Sprintf("ProtoResult was given a nil target (%T)", p.msg), "pass a pointer to a message, such as ProtoResult(&pb.User{})")
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, p.msg)
}

// errorEnvelope holds the fixed fields of a Keel/kratos error response. The envelope has a fixed shape and no
// generated type to decode into, so the standard library parses it, not protojson. The code in the envelope is not
// parsed: decodeError always goes by the actual HTTP status code.
type errorEnvelope struct {
	Reason   string            `json:"reason"`
	Message  string            `json:"message"`
	Metadata map[string]string `json:"metadata"`
}

// contractError is the call error when a 2xx response breaks this client's contract (the content type is not
// accepted, or the result fails to decode). It carries the actual status code, because "the other side returned 200
// but the shape is wrong" must be told apart from "the other side really returned 500" and "never connected";
// AsError takes Code from it.
//
// It is not an *errors.Error: when the caller passes it straight out of its own server, kratos writes the HTTP status
// from Code, so a 2xx error would go out as a successful response. Passed through, it is a plain error here, and
// kratos handles it as 500 as usual, which is exactly the real outcome of this service's hop.
type contractError struct {
	status int
	err    error
}

func (e *contractError) Error() string { return e.err.Error() }
func (e *contractError) Unwrap() error { return e.err }

// skipUnmarshal replaces resty's JSONUnmarshal so that result decoding moves into decodeResult. resty's result parsing
// runs before every OnAfterResponse (where afterResponse is initialised in resty@v2.17.2 client.go), and a failure
// there skips the whole hook chain: the caller gets a bare decoding error (AsError yields 500), logResponse does not
// run, and the log has no status code.
func skipUnmarshal([]byte, any) error { return nil }

// decodeResult decodes a 2xx JSON response into the SetResult target and reports a contractError on failure. It only
// looks at JSON: text + SetResult is already rejected in checkResponseType; 204 is not decoded, consistent with
// resty. Plain structs go through encoding/json; ProtoResult goes to protojson through protoResult.
func decodeResult(_ *resty.Client, resp *resty.Response) error {
	if !resp.IsSuccess() || resp.Request.Result == nil || resp.StatusCode() == http.StatusNoContent ||
		!resty.IsJSONType(resp.Header().Get("Content-Type")) {
		return nil
	}
	if err := json.Unmarshal(resp.Body(), resp.Request.Result); err != nil {
		if errors.Is(err, errNilProtoTarget) {
			return err
		}
		return &contractError{status: resp.StatusCode(),
			err: fmt.Errorf("khttpclient: failed to decode the %d response body: %w", resp.StatusCode(), err)}
	}
	return nil
}

// decodeError restores the error envelope of a non-2xx response into an *errors.Error returned as the call error;
// when the body is not an envelope (such as HTML from a gateway in between), it falls back to the HTTP status code and
// leaves reason empty rather than inventing one.
//
// A SetError target is decoded here on a best-effort basis, and failure is not an error, consistent with resty's own
// handling (parseResponseBody in resty@v2.17.2 middleware.go only warns); resty's JSONUnmarshal has been replaced by
// skipUnmarshal.
func decodeError(_ *resty.Client, resp *resty.Response) error {
	if resp.IsSuccess() {
		return nil
	}
	if resp.Request.Error != nil && resp.IsError() && resty.IsJSONType(resp.Header().Get("Content-Type")) {
		_ = json.Unmarshal(resp.Body(), resp.Request.Error)
	}
	var env errorEnvelope
	if err := json.Unmarshal(resp.Body(), &env); err == nil && (env.Reason != "" || env.Message != "") {
		// Consistent with kratos DefaultErrorDecoder: the actual HTTP status code wins
		e := kerrors.New(resp.StatusCode(), env.Reason, env.Message)
		if len(env.Metadata) > 0 {
			e = e.WithMetadata(env.Metadata)
		}
		return e
	}
	return kerrors.New(resp.StatusCode(), "", http.StatusText(resp.StatusCode()))
}

// checkResponseType guards the two contracts of a 2xx response: the content type must be JSON or plain text, and
// SetResult applies only to JSON.
//
// JSON/text only: this client is for JSON API calls (plain text comes along, such as health checks); a binary
// response means the caller is using it as a download channel, and under the 2MB limit a large file is cut off midway
// while a small one is buffered whole in memory; neither is an "API call". XML is not supported (the Keel stack does
// not produce XML; text/xml gets through because of the text/* prefix, an acceptable corner case).
//
// Text + SetResult is an error: decodeResult decodes only JSON, so a text response would silently leave Result at its
// zero value; the caller gets an empty struct with no error at all, the hardest kind of failure to track down. For
// the raw string use resp.String(), which is the same path for JSON and text.
//
// Non-2xx responses are never checked: error responses go to decodeError to restore the envelope, and an HTML error
// page from a gateway falls back to the status code there, free of these two rules.
//
// It is registered before logResponse: a response judged an error is logged once as ERROR through OnError instead of
// first leaving a status=200 INFO.
func checkResponseType(_ *resty.Client, resp *resty.Response) error {
	if !resp.IsSuccess() || len(resp.Body()) == 0 {
		return nil
	}
	ct := resp.Header().Get("Content-Type")
	mt, _, err := mime.ParseMediaType(ct)
	switch {
	case err == nil && (mt == "application/json" || strings.HasSuffix(mt, "+json")):
		return nil
	case err == nil && strings.HasPrefix(mt, "text/"):
		if resp.Request.Result != nil {
			return &contractError{status: resp.StatusCode(), err: fmt.Errorf(
				"khttpclient: SetResult has no effect on a %s response (only JSON is decoded); use resp.String() for the raw string", mt)}
		}
		return nil
	}
	return &contractError{status: resp.StatusCode(),
		err: fmt.Errorf("khttpclient: unsupported response content type %q (JSON/text only)", ct)}
}

type loggedFlagKey struct{}

// markLoggable attaches a "has this request been logged" flag before the request is sent.
//
// It cannot be inferred from "was a response received": when resty fails while reading the body (decompression
// failure, truncated read), it returns early with the response headers already received and the response middleware
// never ran; RawResponse is non-nil then, and judging "already logged" from it would leave the whole request with no
// log at all. logResponse sets the flag, and logError uses it to log what was missed.
func markLoggable(_ *resty.Client, req *resty.Request) error {
	if f := loggedFlag(req.Context()); f != nil {
		f.Store(false) // retry: reset in place so contexts do not nest on every attempt
		return nil
	}
	req.SetContext(context.WithValue(req.Context(), loggedFlagKey{}, new(atomic.Bool)))
	return nil
}

func loggedFlag(ctx context.Context) *atomic.Bool {
	f, _ := ctx.Value(loggedFlagKey{}).(*atomic.Bool)
	return f
}

// logResponse logs every completed HTTP response (business errors are logged here too, so the errors produced by
// decodeError are not logged again).
func logResponse(service string) resty.ResponseMiddleware {
	return func(_ *resty.Client, resp *resty.Response) error {
		// The same judgement as decodeError: every non-2xx is restored into a call error, so the log level must
		// follow, or a 3xx (redirects turned off, 304) would "return an error yet log INFO".
		lvl := slog.LevelInfo
		if !resp.IsSuccess() {
			lvl = slog.LevelWarn
		}
		slog.Default().Log(resp.Request.Context(), lvl, "http client",
			"service", service,
			"method", resp.Request.Method,
			"url", resp.Request.URL,
			"status", resp.StatusCode(),
			"latency", resp.Time().Seconds(),
		)
		if f := loggedFlag(resp.Request.Context()); f != nil {
			f.Store(true)
		}
		return nil
	}
}

// logError logs the failures logResponse could not: cases with no response such as dial failures and timeouts, and
// cases that failed while reading the body after the response headers arrived, where the response middleware did not
// run. Requests already logged by logResponse (successes, business errors) are skipped here, so every request gets
// exactly one log entry.
func logError(service string) resty.ErrorHook {
	return func(req *resty.Request, err error) {
		if f := loggedFlag(req.Context()); f != nil && f.Load() {
			return
		}
		attrs := []any{
			"service", service,
			"method", req.Method,
			"url", req.URL,
		}
		// When a response was received (the contract check failed, reading the body failed), include the status code;
		// otherwise "the other side returned 200 but it would not decode" looks the same as a dial failure in the log.
		// When there is a response, resty wraps the error in *resty.ResponseError for the error hooks (onErrorHooks in
		// resty@v2.17.2 client.go).
		var re *resty.ResponseError
		if errors.As(err, &re) && re.Response != nil && re.Response.RawResponse != nil {
			attrs = append(attrs, "status", re.Response.StatusCode())
		}
		attrs = append(attrs, "latency", time.Since(req.Time).Seconds(), "error", err)
		slog.ErrorContext(req.Context(), "http client", attrs...)
	}
}
