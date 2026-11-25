// In-package: checks consumer internals (retry delays, stop budget, subject checks) with fake messages.

package knats

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func okHandler(context.Context, *Delivery) HandlerResult { return OK() }

// newTestConsumer builds a consumer attached to no client: stop()/shutdown()/watch() never touch cs.c (only start()
// needs it to connect to NATS), so these methods can be unit-tested entirely without a real connection. Every channel
// is initialised here; missing one ends in a close of nil channel.
func newTestConsumer(durable string) *consumer {
	return &consumer{
		opt:     ConsumerOptions{Stream: NewStream("layout"), Durable: durable},
		stream:  "keel_layout",
		jobs:    make(chan jetstream.Msg),
		done:    make(chan struct{}),
		stopper: newGracefulStop(),
	}
}

// ---- Pure logic: no network and no real connection needed ----

// The naming rule is the only agreement between publisher and subscriber; a mistake makes the two sides miss each
// other without any error, so it must be pinned down.
func TestNaming(t *testing.T) {
	c := &Client{domain: "keel"}

	if got := c.streamName("layout"); got != "keel_layout" {
		t.Errorf("streamName = %q, want keel_layout", got)
	}
	if got := c.subjectPrefix("layout"); got != "keel.layout" {
		t.Errorf("subjectPrefix = %q, want keel.layout", got)
	}
	// Stream names must not contain a dot, or NATS refuses to create the stream.
	if got := c.streamName("a.b"); got == "keel.a.b" {
		t.Error("stream name must not be dot-joined")
	}
}

func TestTrimPrefix(t *testing.T) {
	cases := []struct{ subject, prefix, want string }{
		{"keel.layout.todo.created", "keel.layout", "todo.created"},
		{"keel.layout.todo", "keel.layout", "todo"},
		{"keel.layout", "keel.layout", ""},
		{"other.subject", "keel.layout", "other.subject"},
	}
	for _, c := range cases {
		if got := trimPrefix(c.subject, c.prefix); got != c.want {
			t.Errorf("trimPrefix(%q, %q) = %q, want %q", c.subject, c.prefix, got, c.want)
		}
	}
}

// The backoff table keeps the values from the AMQP era and must be non-decreasing and capped.
func TestRetryDelay(t *testing.T) {
	want := []time.Duration{time.Second, 3 * time.Second, 10 * time.Second, 30 * time.Second}
	for i, w := range want {
		if got := retryDelay(i); got != w {
			t.Errorf("retryDelay(%d) = %v, want %v", i, got, w)
		}
	}
	if got := retryDelay(99); got != 60*time.Second {
		t.Errorf("retryDelay(99) = %v, want 60s", got)
	}
	// A negative value comes from an abnormal NumDelivered; returning a negative delay must not turn the Nak into an
	// immediate redelivery.
	if got := retryDelay(-1); got <= 0 {
		t.Errorf("retryDelay(-1) = %v, must be positive", got)
	}
}

// The server-side ack timeout must exceed the processing time limit by the queueing share, or the message queued in
// the buffer is certain to be judged timed out and redelivered; see the derivation of ackWaitFactor in client.go.
func TestAckWaitFactorHasMargin(t *testing.T) {
	if ackWaitFactor < 2 {
		t.Errorf("ackWaitFactor = %d, must cover at least the processing time of two messages", ackWaitFactor)
	}
}

// The wait limit for closing the connection must stay within 10 seconds: the cleanup phase should not spend much
// budget waiting for one connection to drain; see the drainTimeout comment.
func TestDrainTimeoutWithinTenSeconds(t *testing.T) {
	if drainTimeout > 10*time.Second {
		t.Errorf("drainTimeout = %v, must be within 10 seconds", drainTimeout)
	}
}

func TestSubjectCovered(t *testing.T) {
	cases := []struct {
		have []string
		want string
		ok   bool
	}{
		{[]string{"keel.layout.>"}, "keel.layout.>", true},        // identical
		{[]string{"keel.>"}, "keel.layout.>", true},               // a wider > covers a narrower one
		{[]string{"keel.layout.>"}, "keel.other.>", false},        // different branch
		{[]string{"keel.layout.>"}, "keel.>", false},              // a narrower one cannot cover a wider one
		{[]string{"other.>"}, "keel.layout.>", false},             // unrelated
		{[]string{"a.>", "keel.layout.>"}, "keel.layout.>", true}, // any one of several patterns
		{nil, "keel.layout.>", false},                             // empty list
		// `*` is not considered: the wrong direction is "thought covered, actually receives nothing".
		{[]string{"keel.*.created"}, "keel.layout.>", false},
	}
	for _, c := range cases {
		if got := subjectCovered(c.have, c.want); got != c.ok {
			t.Errorf("subjectCovered(%v, %q) = %v, want %v", c.have, c.want, got, c.ok)
		}
	}
}

func TestHeaderCarrier(t *testing.T) {
	h := nats.Header{}
	carrier := headerCarrier(h)
	carrier.Set("traceparent", "00-abc-def-01")

	if got := carrier.Get("traceparent"); got != "00-abc-def-01" {
		t.Errorf("Get = %q", got)
	}
	if got := carrier.Get("nosuch"); got != "" {
		t.Errorf("missing key must be empty, got %q", got)
	}
	if keys := carrier.Keys(); len(keys) != 1 {
		t.Errorf("Keys = %v, want 1 entry", keys)
	}
}

// A subject repeating the prefix builds a syntactically valid subject nobody publishes to: publishing works and the
// consumer can be created, yet messages vanish with no layer reporting an error. Only the framework can tell, so it
// must fail hard.
func TestSubjectPrefixRejected(t *testing.T) {
	if err := checkSubjectPrefix("keel.layout.todo.created", "keel.layout"); !errors.Is(err, ErrSubjectPrefixed) {
		t.Fatalf("got %v, want ErrSubjectPrefixed", err)
	}
	// A business subject without the prefix passes as usual.
	if err := checkSubjectPrefix("todo.created", "keel.layout"); err != nil {
		t.Fatalf("plain topic rejected: %v", err)
	}
	// The criterion is the full <domain>.<stream>., not the domain alone: keel.sync builds keel.layout.keel.sync,
	// which still matches on both sides, is a legal subject and must not be rejected.
	if err := checkSubjectPrefix("keel.sync", "keel.layout"); err != nil {
		t.Errorf("keel.sync wrongly rejected: %v", err)
	}
	// The same leading characters without reaching the separator must not be rejected.
	if err := checkSubjectPrefix("keel.layoutX.done", "keel.layout"); err != nil {
		t.Errorf("false positive: %v", err)
	}
	// An empty prefix skips the check, so not every subject is rejected.
	if err := checkSubjectPrefix("keel.layout.x", ""); err != nil {
		t.Errorf("empty prefix must not reject: %v", err)
	}
}

// A dotted stream name makes streamName() build a dotted NATS stream name, which nats-server explicitly rejects (see
// the source cited in the checkStreamName comment); only the framework can stop it before a network round trip.
func TestCheckStreamName(t *testing.T) {
	if err := checkStreamName("layout"); err != nil {
		t.Errorf("plain name rejected: %v", err)
	}
	if err := checkStreamName("a.b"); !errors.Is(err, ErrStreamNameDotted) {
		t.Errorf("got %v, want ErrStreamNameDotted", err)
	}
}

// 0 and unset mean "use the default".
func TestStreamOptionsDefaults(t *testing.T) {
	so := (streamOptions{}).normalized()
	if so.MaxAge != defaultStreamMaxAge || so.Replicas != 1 {
		t.Errorf("zero value must fall back to defaults: %+v", so)
	}
}

// A handler panic must not crash the process; it is retried like RetryLater (MaxDeliver bounds it), and
// the reason names the panic so the dead letter advisory says what happened.
func TestSafeHandlePanic(t *testing.T) {
	cs := newTestConsumer("notify")
	cs.handler = func(context.Context, *Delivery) HandlerResult { panic("boom") }
	if res, reason := cs.safeHandle(context.Background(), &Delivery{}); res.kind != resultRetryLater || !strings.Contains(reason, "boom") {
		t.Errorf("panic: got (%v, %q), want RetryLater with the panic value in the reason", res, reason)
	}

	cs.handler = okHandler
	if res, _ := cs.safeHandle(context.Background(), &Delivery{}); res.kind != resultOK {
		t.Errorf("normal handler: got %v, want OK", res)
	}
}

type captureRegistrar struct{ h ConsumerHandler }

func (c *captureRegistrar) Register(h ConsumerHandler, _ ConsumerOptions) { c.h = h }

// A payload that does not decode gives up without calling fn, and the decode error is the give-up reason, so the log and
// the dead letter advisory say why.
func TestEventHandleDecodeFailureReason(t *testing.T) {
	r := &captureRegistrar{}
	called := false
	NewEventType[testEventPayload](NewStream("t"), "created").Handle(r, func(context.Context, testEventPayload, DeliveryInfo) HandlerResult {
		called = true
		return OK()
	}, EventOptions{Durable: "d"})
	cs := newTestConsumer("d")
	cs.handler = r.h
	res, reason := cs.safeHandle(context.Background(), &Delivery{Body: []byte("{not json")})
	if called || res.kind != resultFailed || !strings.Contains(reason, "decode created payload") {
		t.Errorf("got (called=%v, %v, %q), want Failed with the decode error as reason", called, res, reason)
	}
}

// The zero HandlerResult is not a valid disposition; it is retried, the reason says how to fix the handler, and every
// delivery logs an error so the bug shows before the retries run out.
func TestSafeHandleZeroResult(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cs := newTestConsumer("d")
	cs.handler = func(context.Context, *Delivery) HandlerResult { return HandlerResult{} }
	if res, reason := cs.safeHandle(context.Background(), &Delivery{}); res.kind != resultInvalid || !strings.Contains(reason, "zero HandlerResult") {
		t.Errorf("got (%v, %q), want the zero result reported", res, reason)
	}
	if !strings.Contains(buf.String(), "level=ERROR") || !strings.Contains(buf.String(), "returned no result") {
		t.Errorf("log = %q, want an error naming the missing result", buf.String())
	}
}

// fakeMsg implements only the semantics behind the methods process() uses (Metadata/Headers/Data/Subject for reading,
// Ack/NakWithDelay/Term as harmless no-ops), to unit-test process()'s branching without a real connection.
type fakeMsg struct {
	subject string
	header  nats.Header
}

func (m *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{Sequence: jetstream.SequencePair{Stream: 1}, NumDelivered: 1}, nil
}
func (m *fakeMsg) Data() []byte                     { return nil }
func (m *fakeMsg) Headers() nats.Header             { return m.header }
func (m *fakeMsg) Subject() string                  { return m.subject }
func (m *fakeMsg) Reply() string                    { return "" }
func (m *fakeMsg) Ack() error                       { return nil }
func (m *fakeMsg) DoubleAck(context.Context) error  { return nil }
func (m *fakeMsg) Nak() error                       { return nil }
func (m *fakeMsg) NakWithDelay(time.Duration) error { return nil }
func (m *fakeMsg) InProgress() error                { return nil }
func (m *fakeMsg) Term() error                      { return nil }
func (m *fakeMsg) TermWithReason(string) error      { return nil }

// A failed handling is marked failed, retries and give-ups alike; tests asserting spans temporarily replace the global
// TracerProvider and do not run in parallel with other tests.
func TestProcess_MarksSpanFailedRegardlessOfRetryOrGiveUp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result HandlerResult
		want   codes.Code
	}{
		{"ok", OK(), codes.Unset},
		{"retry", RetryLater(errors.New("x")), codes.Error},
		{"giveup", Failed(errors.New("x")), codes.Error},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exp := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
			oldTP := otel.GetTracerProvider()
			otel.SetTracerProvider(tp)
			t.Cleanup(func() {
				_ = tp.Shutdown(context.Background())
				otel.SetTracerProvider(oldTP)
			})

			cs := &consumer{
				c:       &Client{domain: "keel"},
				opt:     ConsumerOptions{Stream: NewStream("layout"), Durable: "notify", MaxProcessTime: 10 * time.Second},
				handler: func(context.Context, *Delivery) HandlerResult { return tc.result },
			}
			cs.process(context.Background(), &fakeMsg{subject: "keel.layout.todo.created", header: nats.Header{}})

			spans := exp.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("got %d spans, want 1", len(spans))
			}
			if spans[0].Status.Code != tc.want {
				t.Errorf("span status = %v, want %v", spans[0].Status.Code, tc.want)
			}
		})
	}
}

// fakeConsumeContext only drives watch's branching; it does not simulate nats.go's behaviour.
type fakeConsumeContext struct{ closed chan struct{} }

func (f *fakeConsumeContext) Stop()  { close(f.closed) }
func (f *fakeConsumeContext) Drain() { close(f.closed) }

func (f *fakeConsumeContext) Closed() <-chan struct{} { return f.closed }

// A subscription that closes by itself without anyone asking (durable deleted, consumer kicked) must take the whole
// consumer server down: a live process whose messages nobody consumes is the hardest failure to notice. A
// subscription we closed ourselves while stopping must not report an error. failures is passed in by the caller
// (ConsumerServer); here a local channel drives it directly.
func TestConsumerDeathIsReported(t *testing.T) {
	cs := newTestConsumer("notify")
	failures := make(chan error, 1)

	cc := &fakeConsumeContext{closed: make(chan struct{})}
	done := make(chan struct{})
	go func() { cs.watch(cc, failures); close(done) }()

	cc.Stop() // simulate the server terminating the subscription
	<-done

	select {
	case err := <-failures:
		if err == nil {
			t.Fatal("want an error describing the dead consumer")
		}
	default:
		t.Fatal("consumer died silently: nothing on failures")
	}

	// Now we stop it ourselves: watch must stay quiet, or every graceful stop would report a false failure.
	cs2 := newTestConsumer("notify")
	cs2.stopped = true
	failures2 := make(chan error, 1)
	cc2 := &fakeConsumeContext{closed: make(chan struct{})}
	done2 := make(chan struct{})
	go func() { cs2.watch(cc2, failures2); close(done2) }()
	cc2.Stop()
	<-done2

	select {
	case err := <-failures2:
		t.Fatalf("a normal stop must not report a failure: %v", err)
	default:
	}
}

// failures must be buffered: watch reports from its own goroutine, which must not hang when nobody receives.
func TestFailuresChannelDoesNotBlockWatchers(t *testing.T) {
	failures := make(chan error, 1)
	for i := range 3 {
		cs := newTestConsumer("d")
		cc := &fakeConsumeContext{closed: make(chan struct{})}
		done := make(chan struct{})
		go func() { cs.watch(cc, failures); close(done) }()
		cc.Stop()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("watcher %d is stuck reporting", i)
		}
	}
}

// Stopping must not be held up by a stuck handler: every caller must be able to return within **its own** budget.
//
// The counter-example puts the whole blocking flow inside sync.Once.Do: the first caller in decides everything and
// later ones can only wait, which is exactly how kratos's stopTimeout gets bypassed.
func TestStopHonorsEachCallerBudget(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)

	cs := newTestConsumer("stuck")
	cs.wg.Go(func() { <-blocked }) // a worker that ignores ctx.Done() and lingers

	// The caller that starts the stop first holds a long budget.
	longCtx, cancelLong := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelLong()
	go cs.stop(longCtx)

	// The later caller has only a short budget and must return by its own deadline, not be blocked by the Once or the
	// earlier caller.
	shortCtx, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	returned := make(chan struct{})
	go func() { cs.stop(shortCtx); close(returned) }()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the caller is blocked; its own stop budget is meaningless")
	}
}

// The stop budget must keep the caller's deadline and not be cut short by the internal fallback.
func TestStopBudgetKeepsCallerDeadline(t *testing.T) {
	caller, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	budget, cancelBudget := stopBudget(caller)
	defer cancelBudget()

	dl, ok := budget.Deadline()
	if !ok {
		t.Fatal("the budget lost its deadline")
	}
	if want, _ := caller.Deadline(); !dl.Equal(want) {
		t.Errorf("deadline = %v, want %v (the caller gave 3 minutes, which must not be cut short by the internal fallback)", dl, want)
	}
	// A caller letting go must not interrupt the background flow: other callers may still be waiting.
	cancel()
	if budget.Err() != nil {
		t.Error("the caller's cancel must not affect the stop budget")
	}

	// The fallback applies only without a deadline.
	fallback, cancelFallback := stopBudget(context.Background())
	defer cancelFallback()
	if _, ok := fallback.Deadline(); !ok {
		t.Error("a caller without a deadline must get the fallback deadline, or a stuck handler would never stop")
	}
}

// A stop given up halfway must not report success, or the caller believes every message was handled cleanly. This
// tests ConsumerServer.Stop (it owns the subscriptions' stop budget, not client.Shutdown; only the cleanup function
// closes the connection) by constructing a ConsumerServer literal directly: it is the real type
// NewConsumerServer produces, and the only step skipped here is "open a real connection and register"; Stop's logic
// does not depend on being connected.
//
// Run this with `-race -count=100`: once the result depends on state the background flow writes later, a single run
// often happens to win the race and hides the problem.
func TestConsumerServerStop_ReportsAbandonment(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)

	cs := newTestConsumer("stuck")
	cs.wg.Go(func() { <-blocked }) // a worker that ignores ctx.Done()

	srv := &ConsumerServer{
		consumers: []*consumer{cs},
		failures:  make(chan error, 1),
		stopping:  make(chan struct{}),
		stopper:   newGracefulStop(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := srv.Stop(ctx); err == nil {
		t.Fatal("gave up waiting but reported success")
	}
}

// ---- Registration checks: through the public NewConsumerServer + ConsumerRegistrar, with no real connection needed
// (checks happen before any network I/O), but still using the *Client the constructor itself produces, not forged
// internal state. ----

func TestConsumerRegistration_ValidationErrors(t *testing.T) {
	c := newClient("keel", "test", streamOptions{MaxAge: defaultStreamMaxAge, Replicas: 1})

	if _, err := NewConsumerServer(c, func(r ConsumerRegistrar) {
		r.Register(okHandler, ConsumerOptions{Stream: NewStream("layout")}) // empty durable
	}); !errors.Is(err, ErrEmptyDurable) {
		t.Errorf("empty durable: got %v, want ErrEmptyDurable", err)
	}

	if _, err := NewConsumerServer(c, func(r ConsumerRegistrar) {
		r.Register(okHandler, ConsumerOptions{Durable: "notify"}) // empty stream
	}); !errors.Is(err, ErrEmptyStream) {
		t.Errorf("empty stream: got %v, want ErrEmptyStream", err)
	}

	if _, err := NewConsumerServer(c, func(r ConsumerRegistrar) {
		r.Register(nil, ConsumerOptions{Stream: NewStream("s"), Durable: "d"})
	}); !errors.Is(err, ErrNilHandler) {
		t.Errorf("nil handler: got %v, want ErrNilHandler", err)
	}

	// A duplicate durable makes two handlers compete for one consumption progress, each getting half of the messages.
	if _, err := NewConsumerServer(c, func(r ConsumerRegistrar) {
		opt := ConsumerOptions{Stream: NewStream("layout"), Durable: "notify"}
		r.Register(okHandler, opt)
		r.Register(okHandler, opt)
	}); !errors.Is(err, ErrDuplicateDurable) {
		t.Errorf("duplicate durable: got %v, want ErrDuplicateDurable", err)
	}

	// The same durable name on different streams is legal.
	if _, err := NewConsumerServer(c, func(r ConsumerRegistrar) {
		r.Register(okHandler, ConsumerOptions{Stream: NewStream("layout"), Durable: "notify"})
		r.Register(okHandler, ConsumerOptions{Stream: NewStream("other"), Durable: "notify"})
	}); err != nil {
		t.Errorf("same durable on another stream: %v", err)
	}

	// Several validation failures must be accumulated and returned together, not "short-circuit on the first error",
	// consistent with mw.Chain.Err()'s accumulation rule.
	if _, err := NewConsumerServer(c, func(r ConsumerRegistrar) {
		r.Register(okHandler, ConsumerOptions{Stream: NewStream("s"), Durable: "d1", Workers: -1})
		r.Register(okHandler, ConsumerOptions{Stream: NewStream("s"), Durable: "d2", MaxProcessTime: -time.Second})
	}); !errors.Is(err, ErrNegativeOption) || strings.Count(err.Error(), "keel/knats.option-negative") < 2 {
		t.Errorf("both negative-option errors should be accumulated, got: %v", err)
	}

	if _, err := NewConsumerServer(c, func(r ConsumerRegistrar) {
		r.Register(okHandler, ConsumerOptions{Stream: NewStream("layout"), Durable: "notify", Subject: "keel.layout.todo.created"})
	}); !errors.Is(err, ErrSubjectPrefixed) {
		t.Errorf("subject prefixed: got %v, want ErrSubjectPrefixed", err)
	}

	// A dotted stream name: streamName() would build a stream name NATS rejects.
	if _, err := NewConsumerServer(c, func(r ConsumerRegistrar) {
		r.Register(okHandler, ConsumerOptions{Stream: NewStream("a.b"), Durable: "notify"})
	}); !errors.Is(err, ErrStreamNameDotted) {
		t.Errorf("dotted stream name: got %v, want ErrStreamNameDotted", err)
	}

	// No consumer registered at all: setup is nil, or it ran without calling Register.
	if _, err := NewConsumerServer(c, nil); !errors.Is(err, ErrNoConsumers) {
		t.Errorf("nil setup: got %v, want ErrNoConsumers", err)
	}
	if _, err := NewConsumerServer(c, func(ConsumerRegistrar) {}); !errors.Is(err, ErrNoConsumers) {
		t.Errorf("no Register calls: got %v, want ErrNoConsumers", err)
	}
}

type testEventPayload struct {
	Msg string
}
