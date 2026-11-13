package knats_test

import (
	"errors"
	"testing"

	"github.com/go-keel/keel/core/knats"
)

func TestHandlerResultString(t *testing.T) {
	for _, tc := range []struct {
		r    knats.HandlerResult
		want string
	}{
		{knats.OK(), "OK"},
		{knats.RetryLater(errors.New("redis down")), "RetryLater: redis down"},
		{knats.Failed(errors.New("bad id")), "Failed: bad id"},
		{knats.Failed(nil), "Failed"},
		{knats.HandlerResult{}, "invalid zero HandlerResult"},
	} {
		if got := tc.r.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}
