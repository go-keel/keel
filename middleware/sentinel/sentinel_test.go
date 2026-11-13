package sentinel_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	kerrors "github.com/go-kratos/kratos/v3/errors"

	"github.com/go-keel/keel/middleware/sentinel"
)

var (
	errNotFound = errors.New("record not found")
	errUnknown  = errors.New("unexpected")
)

func TestGroup_On_Matches(t *testing.T) {
	sg := sentinel.NewGroup().On(errNotFound, sentinel.NotFound("NOT_FOUND"))
	h := sg.Middleware()(func(ctx context.Context, req any) (any, error) {
		return nil, errNotFound
	})
	_, err := h(context.Background(), nil)
	ke := kerrors.FromError(err)
	if ke == nil || ke.Code != 404 {
		t.Fatalf("expected 404, got %v", err)
	}
}

func TestGroup_DefaultUnmatched(t *testing.T) {
	sg := sentinel.NewGroup().
		On(errNotFound, sentinel.NotFound("NOT_FOUND")).
		Default(func(ctx context.Context, err error) error {
			return kerrors.InternalServer("INTERNAL", "boom")
		})
	h := sg.Middleware()(func(ctx context.Context, req any) (any, error) {
		return nil, errUnknown
	})
	_, err := h(context.Background(), nil)
	ke := kerrors.FromError(err)
	if ke == nil || ke.Code != 500 {
		t.Fatalf("expected 500, got %v", err)
	}
}

func TestGroup_NoErrorPassesThrough(t *testing.T) {
	sg := sentinel.NewGroup()
	h := sg.Middleware()(func(ctx context.Context, req any) (any, error) {
		return "ok", nil
	})
	resp, err := h(context.Background(), nil)
	if err != nil || resp != "ok" {
		t.Fatalf("expected ok/nil, got %v/%v", resp, err)
	}
}

// A wrapped sentinel still matches via errors.Is, but the outgoing Message must be the sentinel's own sentence:
// the context the caller added (here the table name) is for developers and must not reach the message users read.
func TestConvenience_WrappedSentinelKeepsSentinelMessage(t *testing.T) {
	sg := sentinel.NewGroup().On(errNotFound, sentinel.NotFound("NOT_FOUND"))
	h := sg.Middleware()(func(ctx context.Context, req any) (any, error) {
		return nil, fmt.Errorf("query internal_account: %w", errNotFound)
	})
	_, err := h(context.Background(), nil)
	ke := kerrors.FromError(err)
	if ke.Message != "record not found" {
		t.Fatalf("message = %q, want %q", ke.Message, "record not found")
	}
	if strings.Contains(ke.Message, "internal_account") {
		t.Fatalf("internal detail leaked into message: %q", ke.Message)
	}
}

// 412 is the outgoing wire shape: users map it to and from gRPC FAILED_PRECONDITION, so changing the code breaks the contract.
func TestPreconditionFailed_Maps412(t *testing.T) {
	errInUse := errors.New("category in use")
	sg := sentinel.NewGroup().On(errInUse, sentinel.PreconditionFailed("CATEGORY_IN_USE"))
	h := sg.Middleware()(func(ctx context.Context, req any) (any, error) {
		return nil, fmt.Errorf("delete category: %w", errInUse)
	})
	_, err := h(context.Background(), nil)
	ke := kerrors.FromError(err)
	if ke.Code != 412 || ke.Reason != "CATEGORY_IN_USE" || ke.Message != "category in use" {
		t.Fatalf("got code=%d reason=%q message=%q", ke.Code, ke.Reason, ke.Message)
	}
}

// A registration that could never run is recorded and returned by Err, so it fails startup instead of being dropped.
func TestGroup_InvalidRegistrationRecordsError(t *testing.T) {
	cases := map[string]func(*sentinel.Group){
		"On nil sentinel":       func(g *sentinel.Group) { g.On(nil, sentinel.NotFound("NOTE_NOT_FOUND")) },
		"On nil handler":        func(g *sentinel.Group) { g.On(errNotFound, nil) },
		"OnLocaled nil handler": func(g *sentinel.Group) { g.OnLocaled(errNotFound, nil) },
		"Default nil handler":   func(g *sentinel.Group) { g.Default(nil) },
	}
	for name, register := range cases {
		t.Run(name, func(t *testing.T) {
			g := sentinel.NewGroup()
			register(g)
			if err := g.Err(); err == nil || !strings.Contains(err.Error(), "keel/sentinel.rule-invalid:") {
				t.Fatalf("Err() = %v, want rule sentinel.rule-invalid", err)
			}
		})
	}
	if err := sentinel.NewGroup().On(errNotFound, sentinel.NotFound("NOTE_NOT_FOUND")).Err(); err != nil {
		t.Fatalf("valid registration: Err() = %v, want nil", err)
	}
}
