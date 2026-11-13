package mw

import (
	"errors"
	"fmt"

	"github.com/go-kratos/kratos/v3/middleware"

	"github.com/go-keel/keel/internal/authslot"
	"github.com/go-keel/keel/internal/errs"
)

// Middleware is an alias of kratos middleware.Middleware, not a new type. Defining a function type as a new type
// would force users to write a conversion for every kratos ecosystem middleware they plug in (otelmetrics,
// circuitbreaker, the official recovery/validate...); an alias is identical to the original type, so both Keel's
// own middleware and plain kratos middleware pass straight to Chain's methods with zero friction. See ADR keel-0010.
type Middleware = middleware.Middleware

// Server-side slot names
const (
	SlotRecovery  = "recovery"
	SlotTracing   = "tracing"
	SlotMetrics   = "metrics"
	SlotLogging   = "logging"
	SlotMetadata  = "metadata"
	SlotLocale    = "locale"
	SlotAuth      = "auth"
	SlotValidate  = "validate"
	SlotSentinel  = "sentinel"
	SlotRatelimit = "ratelimit"
)

// Client-side slot names
const (
	ClientSlotRecovery     = "recovery"
	ClientSlotTracing      = "tracing"
	ClientSlotMetrics      = "metrics"
	ClientSlotMetadata     = "metadata"
	ClientSlotAuth         = "auth"
	ClientSlotCircuitBreak = "circuitbreaker"
)

type namedSlot struct {
	name string
	mw   Middleware
}

// insertSlots inserts middleware before (offset 0) or after (offset 1) the
// named slot. When the target slot is missing it does not panic but returns the original slots and an error: startup
// errors are always returned by constructors, and panicking here would bypass that rule.
func insertSlots(slots []namedSlot, slot string, m Middleware, offset int, name ...string) ([]namedSlot, error) {
	n := ""
	if len(name) > 0 {
		n = name[0]
	}
	if n != "" && hasSlot(slots, n) {
		return slots, errSlotDuplicate(n)
	}
	for i := range slots {
		if slots[i].name == slot {
			at := i + offset
			slots = append(slots, namedSlot{})
			copy(slots[at+1:], slots[at:])
			slots[at] = namedSlot{name: n, mw: m}
			return slots, nil
		}
	}
	return slots, errs.Rules.New("mw.slot-not-found", fmt.Sprintf("mw chain has no slot named %q", slot), "use one of the Slot* constants or a name given to an earlier InsertAfter/InsertBefore, and check the chain was built from the preset that has that slot")
}

// hasSlot reports whether slots already has a slot with this name.
func hasSlot(slots []namedSlot, name string) bool {
	for _, s := range slots {
		if s.name == name {
			return true
		}
	}
	return false
}

// errSlotDuplicate reports a second slot with the same name: a preset mounted twice would run its middleware twice,
// and by-name operations would only ever reach the first copy.
func errSlotDuplicate(name string) error {
	return errs.Rules.New("mw.slot-duplicate", fmt.Sprintf("mw chain already has a slot named %q", name), "mount each preset once per chain, and give each inserted middleware a name not used by another slot")
}

// Chain is an ordered, named collection of server-side middleware.
type Chain struct {
	slots []namedSlot
	// err accumulates assembly-time errors: failed construction of Default's own middleware and failed by-name
	// operations (insert/replace target slot missing, a duplicate slot name, filling a missing auth slot). It does not
	// panic here: during assembly a chain may be processed by several combinators in a row, and reporting every
	// problem at once is easier to debug than stopping at the first panic. Server constructors and
	// keel.NewServiceClient retrieve and return it once via Err().
	err error
}

// empty returns an empty Chain; chains start from Default or Minimal.
func empty() *Chain { return &Chain{} }

// Err returns the accumulated chain-construction error, if any. Server
// constructors (api.NewHTTPServer / NewGRPCServer) and
// keel.NewServiceClient check this before building.
func (c *Chain) Err() error { return c.err }

// add appends a named slot. Framework middleware is mounted only through Default, Minimal and the With* presets. A
// name already in the chain records an error and leaves the chain unchanged.
func (c *Chain) add(slot string, m Middleware) *Chain {
	if hasSlot(c.slots, slot) {
		c.err = errors.Join(c.err, errSlotDuplicate(slot))
		return c
	}
	c.slots = append(c.slots, namedSlot{name: slot, mw: m})
	return c
}

// reserveAuth adds the auth slot as a pass-through placeholder, so servers without authentication work as usual and
// the slot stays an anchor for InsertAfter / InsertBefore. Only Default calls it.
func (c *Chain) reserveAuth() *Chain {
	return c.add(SlotAuth, passthrough)
}

func passthrough(h middleware.Handler) middleware.Handler { return h }

func init() {
	authslot.Fill = func(chain any, m Middleware) { chain.(*Chain).fillAuth(m) }
}

// fillAuth puts the auth middleware a server built from its own auth config into the auth slot. It is reachable only
// through internal/authslot from the api server constructors, which call it once on their own Clone() of the chain,
// so authentication has exactly one way in: the server's auth config. A chain with no auth slot (built from Minimal)
// records an error, because the configured auth would otherwise silently not run.
func (c *Chain) fillAuth(m Middleware) {
	for i := range c.slots {
		if c.slots[i].name == SlotAuth {
			c.slots[i].mw = m
			return
		}
	}
	c.err = errors.Join(c.err, errs.Rules.New("mw.no-auth-slot", "the server has auth configured but its middleware chain has no auth slot, such as one built from mw.Minimal", "build the server chain from mw.Default, or remove the auth section from this server's config"))
}

// replace replaces an existing slot's middleware. A missing slot records an error rather than panicking.
func (c *Chain) replace(slot string, m Middleware) *Chain {
	for i := range c.slots {
		if c.slots[i].name == slot {
			c.slots[i].mw = m
			return c
		}
	}
	c.err = errors.Join(c.err, errs.Rules.New("mw.slot-not-found", fmt.Sprintf("mw chain has no slot named %q", slot), "use one of the Slot* constants or a name given to an earlier InsertAfter/InsertBefore, and check the chain was built from the preset that has that slot"))
	return c
}

// InsertAfter inserts middleware after the named slot. Optional name makes
// the inserted slot addressable as an InsertAfter/InsertBefore anchor. A missing target records an error and leaves the chain unchanged.
func (c *Chain) InsertAfter(slot string, m Middleware, name ...string) {
	slots, err := insertSlots(c.slots, slot, m, 1, name...)
	c.slots = slots
	if err != nil {
		c.err = errors.Join(c.err, err)
	}
}

// InsertBefore inserts middleware before the named slot. A missing target records an error and leaves the chain unchanged.
func (c *Chain) InsertBefore(slot string, m Middleware, name ...string) {
	slots, err := insertSlots(c.slots, slot, m, 0, name...)
	c.slots = slots
	if err != nil {
		c.err = errors.Join(c.err, err)
	}
}

// Clone returns an independent copy of the chain, including its accumulated errors. Use it when
// deriving per-server variants from a shared base: InsertAfter, InsertBefore and the With* presets modify the
// chain in place and return nothing, so two servers customizing the same chain instance would corrupt each other.
func (c *Chain) Clone() *Chain {
	slots := make([]namedSlot, len(c.slots))
	copy(slots, c.slots)
	return &Chain{slots: slots, err: c.err}
}

// Build returns the ordered middleware slice.
func (c *Chain) Build() []Middleware {
	result := make([]Middleware, 0, len(c.slots))
	for _, s := range c.slots {
		result = append(result, s.mw)
	}
	return result
}

// ClientChain is an ordered, named collection of client-side middleware.
type ClientChain struct {
	slots []namedSlot
	err   error // same meaning as Chain.err; there is no auth slot reserve/fill mechanism, client auth is still added manually.
}

// clientEmpty returns an empty ClientChain.
func clientEmpty() *ClientChain { return &ClientChain{} }

// Err returns the accumulated chain-construction error, if any. Checked by
// keel.NewServiceClient before building the client.
func (c *ClientChain) Err() error { return c.err }

// add appends a named slot; a name already in the chain records an error and leaves the chain unchanged.
func (c *ClientChain) add(slot string, m Middleware) *ClientChain {
	if hasSlot(c.slots, slot) {
		c.err = errors.Join(c.err, errSlotDuplicate(slot))
		return c
	}
	c.slots = append(c.slots, namedSlot{name: slot, mw: m})
	return c
}

// InsertAfter inserts middleware after the named slot. A missing target records an error and leaves the chain unchanged.
func (c *ClientChain) InsertAfter(slot string, m Middleware, name ...string) {
	slots, err := insertSlots(c.slots, slot, m, 1, name...)
	c.slots = slots
	if err != nil {
		c.err = errors.Join(c.err, err)
	}
}

// InsertBefore inserts middleware before the named slot. A missing target records an error and leaves the chain unchanged.
func (c *ClientChain) InsertBefore(slot string, m Middleware, name ...string) {
	slots, err := insertSlots(c.slots, slot, m, 0, name...)
	c.slots = slots
	if err != nil {
		c.err = errors.Join(c.err, err)
	}
}

// Clone returns an independent copy of the chain (see Chain.Clone).
func (c *ClientChain) Clone() *ClientChain {
	slots := make([]namedSlot, len(c.slots))
	copy(slots, c.slots)
	return &ClientChain{slots: slots, err: c.err}
}

// Build returns the ordered middleware slice.
func (c *ClientChain) Build() []Middleware {
	result := make([]Middleware, 0, len(c.slots))
	for _, s := range c.slots {
		result = append(result, s.mw)
	}
	return result
}
