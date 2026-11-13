// In-package: checks slot order and recorded errors on chains built from the unexported empty constructors.

package mw

import (
	"context"
	"testing"

	"github.com/go-kratos/kratos/v3/middleware"
)

func recorder(name string, order *[]string) middleware.Middleware {
	return func(h middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			*order = append(*order, name)
			return h(ctx, req)
		}
	}
}

func buildOrder(t *testing.T, build func(order *[]string) *Chain) []string {
	t.Helper()
	var order []string
	chain := build(&order)
	mws := chain.Build()
	h := func(ctx context.Context, req any) (any, error) { return nil, nil }
	wrapped := middleware.Chain(mws...)(h)
	_, _ = wrapped(context.Background(), nil)
	return order
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("order mismatch at %d: got %v want %v", i, got, want)
		}
	}
}

func TestChain_Order(t *testing.T) {
	got := buildOrder(t, func(o *[]string) *Chain {
		return empty().
			add(SlotRecovery, recorder("recovery", o)).
			add(SlotAuth, recorder("auth", o)).
			add(SlotValidate, recorder("validate", o))
	})
	eq(t, got, []string{"recovery", "auth", "validate"})
}

func TestChain_Replace(t *testing.T) {
	got := buildOrder(t, func(o *[]string) *Chain {
		c := empty().add(SlotAuth, recorder("original", o))
		c.replace(SlotAuth, recorder("replaced", o))
		return c
	})
	eq(t, got, []string{"replaced"})
}

// Startup errors have only one exit: a failed by-name operation does not panic but is recorded on the chain and retrieved via Err().
func TestChain_ReplaceMissingRecordsError(t *testing.T) {
	c := empty().replace(SlotAuth, recorder("x", nil))
	if c.Err() == nil {
		t.Fatal("expected Err() to be set after Replace of missing slot")
	}
}

func TestChain_InsertAfter(t *testing.T) {
	got := buildOrder(t, func(o *[]string) *Chain {
		c := empty().
			add(SlotRecovery, recorder("recovery", o)).
			add(SlotAuth, recorder("auth", o))
		c.InsertAfter(SlotRecovery, recorder("tracing", o))
		return c
	})
	eq(t, got, []string{"recovery", "tracing", "auth"})
}

func TestChain_InsertBefore(t *testing.T) {
	got := buildOrder(t, func(o *[]string) *Chain {
		c := empty().
			add(SlotRecovery, recorder("recovery", o)).
			add(SlotAuth, recorder("auth", o))
		c.InsertBefore(SlotAuth, recorder("rbac", o))
		return c
	})
	eq(t, got, []string{"recovery", "rbac", "auth"})
}

func TestClientChain_Build(t *testing.T) {
	c := clientEmpty().
		add(ClientSlotTracing, recorder("tracing", nil)).
		add(ClientSlotMetadata, recorder("metadata", nil))
	if n := len(c.Build()); n != 2 {
		t.Fatalf("expected 2 client middlewares, got %d", n)
	}
}

func TestClientChain_InsertBeforeAfter(t *testing.T) {
	var order []string
	c := clientEmpty().
		add(ClientSlotRecovery, recorder("recovery", &order)).
		add(ClientSlotMetadata, recorder("metadata", &order))
	c.InsertAfter(ClientSlotRecovery, recorder("tracing", &order), ClientSlotTracing)
	c.InsertBefore(ClientSlotMetadata, recorder("metrics", &order), ClientSlotMetrics)

	h := func(ctx context.Context, req any) (any, error) { return nil, nil }
	wrapped := middleware.Chain(c.Build()...)(h)
	_, _ = wrapped(context.Background(), nil)
	eq(t, order, []string{"recovery", "tracing", "metrics", "metadata"})
}

func TestClientChain_InsertMissingRecordsError(t *testing.T) {
	c := clientEmpty()
	c.InsertAfter("nope", nil)
	if c.Err() == nil {
		t.Fatal("expected Err() to be set for missing slot")
	}
}

// A name already in the chain is refused by every way of adding a slot; unnamed inserts never collide.
func TestChain_DuplicateSlotNameRecordsError(t *testing.T) {
	cases := map[string]func(*Chain){
		"add":          func(c *Chain) { c.add("a", recorder("x", nil)) },
		"InsertAfter":  func(c *Chain) { c.InsertAfter("a", recorder("x", nil), "b") },
		"InsertBefore": func(c *Chain) { c.InsertBefore("b", recorder("x", nil), "a") },
	}
	for name, op := range cases {
		t.Run(name, func(t *testing.T) {
			c := empty().add("a", recorder("a", nil)).add("b", recorder("b", nil))
			op(c)
			wantRule(t, c.Err(), "mw.slot-duplicate")
			if len(c.slots) != 2 {
				t.Fatalf("slots = %d, want 2 (chain unchanged)", len(c.slots))
			}
		})
	}
	c := empty().add("a", recorder("a", nil))
	c.InsertAfter("a", recorder("x", nil))
	c.InsertAfter("a", recorder("y", nil))
	if c.Err() != nil || len(c.slots) != 3 {
		t.Fatalf("unnamed inserts: Err() = %v, slots = %d, want nil and 3", c.Err(), len(c.slots))
	}
}

func TestClientChain_DuplicateSlotNameRecordsError(t *testing.T) {
	c := clientEmpty()
	WithClientAuth(c, func() string { return "" })
	WithClientAuth(c, func() string { return "" })
	wantRule(t, c.Err(), "mw.slot-duplicate")
}

func TestChain_ErrAccumulatesAcrossOps(t *testing.T) {
	c := empty()
	c.replace("missing-a", recorder("x", nil))
	c.InsertAfter("missing-b", recorder("y", nil))
	if c.Err() == nil {
		t.Fatal("expected accumulated error from both failed ops")
	}
}

// Clone() copies the accumulated errors: the copies two servers each Clone must both see problems already on the
// base chain, rather than only the original chain being able to report them.
func TestChain_CloneCarriesErr(t *testing.T) {
	base := empty().replace("missing", recorder("x", nil))
	if base.Err() == nil {
		t.Fatal("setup: expected base to already have an error")
	}
	if base.Clone().Err() == nil {
		t.Fatal("Clone should carry forward the accumulated error")
	}
}

func TestChain_CloneIndependent(t *testing.T) {
	var order []string
	base := empty().
		add("a", recorder("a", &order)).
		add("b", recorder("b", &order))

	c1 := base.Clone()
	c1.InsertAfter("a", recorder("x", &order), "x")
	c2 := base.Clone()
	c2.InsertBefore("a", recorder("y", &order), "y")

	if got := len(base.Build()); got != 2 {
		t.Errorf("base mutated: %d slots, want 2", got)
	}
	if got := len(c1.Build()); got != 3 {
		t.Errorf("c1 = %d slots, want 3", got)
	}
	if got := len(c2.Build()); got != 3 {
		t.Errorf("c2 = %d slots, want 3", got)
	}
}

func TestClientChain_CloneIndependent(t *testing.T) {
	var order []string
	base := clientEmpty().add("a", recorder("a", &order))
	derived := base.Clone().add("b", recorder("b", &order))

	if got := len(base.Build()); got != 1 {
		t.Errorf("base mutated: %d slots, want 1", got)
	}
	if got := len(derived.Build()); got != 2 {
		t.Errorf("derived = %d slots, want 2", got)
	}
}
