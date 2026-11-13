package mw_test

import (
	"context"
	"strings"
	"testing"

	kmiddleware "github.com/go-kratos/kratos/v3/middleware"

	"github.com/go-keel/keel/internal/authslot"
	"github.com/go-keel/keel/mw"
)

// The auth slot Default reserves is a placeholder: the chain builds and runs normally even if no server fills it
// (servers without authentication work as usual), and no error has been recorded yet.
func TestDefault_ReservesEmptyAuthSlot(t *testing.T) {
	chain := mw.Default(nil)
	if chain.Err() != nil {
		t.Fatalf("fresh Default() chain should have no error, got %v", chain.Err())
	}
	h := func(ctx context.Context, req any) (any, error) { return "ok", nil }
	if _, err := kmiddleware.Chain(chain.Build()...)(h)(context.Background(), nil); err != nil {
		t.Fatalf("chain with unfilled auth slot should still execute, got %v", err)
	}
}

// fillAuth puts the auth middleware into the placeholder; once filled, the slot takes effect.
func TestFillAuth_FillsAuthSlot(t *testing.T) {
	var hit bool
	authMW := func(h kmiddleware.Handler) kmiddleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			hit = true
			return h(ctx, req)
		}
	}
	chain := mw.Default(nil)
	authslot.Fill(chain, authMW)
	if chain.Err() != nil {
		t.Fatalf("filling the auth slot should not error, got %v", chain.Err())
	}
	h := func(ctx context.Context, req any) (any, error) { return "ok", nil }
	if _, err := kmiddleware.Chain(chain.Build()...)(h)(context.Background(), nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hit {
		t.Fatal("filled auth middleware should have run")
	}
}

// A chain with no auth slot (built from Minimal()) records mw.no-auth-slot instead of silently dropping
// authentication or panicking.
func TestFillAuth_NoAuthSlotRecordsError(t *testing.T) {
	chain := mw.Minimal()
	authslot.Fill(chain, func(h kmiddleware.Handler) kmiddleware.Handler { return h })
	if err := chain.Err(); err == nil || !strings.Contains(err.Error(), "mw.no-auth-slot") {
		t.Fatalf("expected mw.no-auth-slot, got %v", err)
	}
}
