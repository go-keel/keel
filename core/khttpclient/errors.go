package khttpclient

import (
	"errors"

	kerrors "github.com/go-kratos/kratos/v3/errors"
)

// ErrorInfo is the structured view of the error envelope of a failed call. Callers read the business error code
// through it without touching the kratos errors API.
type ErrorInfo struct {
	Code     int               // HTTP status code
	Reason   string            // business error code (the reason string); branch on this
	Message  string            // human-readable description; do not use it for program logic
	Metadata map[string]string // additional context
}

// AsError converts a call error returned by this client into the envelope view; when err is nil, ok is false.
// Code is the actual status code: so for a non-2xx response that is not an envelope, and so for a 2xx response that
// breaks the contract (the result fails to decode, or the content type is not accepted), where Code is that 2xx and
// Message is the reason it was judged an error. A transport failure (no response) has no status code and gets
// Code=500. In all three cases Reason is empty: without a reliable reason, none is invented.
func AsError(err error) (ErrorInfo, bool) {
	if err == nil {
		return ErrorInfo{}, false
	}
	var ce *contractError
	if errors.As(err, &ce) {
		return ErrorInfo{Code: ce.status, Message: ce.Error()}, true
	}
	e := kerrors.FromError(err)
	return ErrorInfo{
		Code:     int(e.Code),
		Reason:   e.Reason,
		Message:  e.Message,
		Metadata: e.Metadata,
	}, true
}

// IsReason reports whether a call error carries the given business reason; it is the recommended entry point for
// branching on business errors. An empty reason always yields false, so it never matches "no reason" by mistake.
func IsReason(err error, reason string) bool {
	if reason == "" {
		return false
	}
	info, ok := AsError(err)
	return ok && info.Reason == reason
}
