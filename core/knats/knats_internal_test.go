// In-package: checks consumer internals (retry delays, stop budget, subject checks) with fake messages.

package knats

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-kratos/kratos/v3/transport"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
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

// The zero HandlerResult is not a valid disposition; it is retried and the reason says how to fix the handler.
func TestSafeHandleZeroResult(t *testing.T) {
	cs := newTestConsumer("d")
	cs.handler = func(context.Context, *Delivery) HandlerResult { return HandlerResult{} }
	if res, reason := cs.safeHandle(context.Background(), &Delivery{}); res.kind != resultInvalid || !strings.Contains(reason, "zero HandlerResult") {
		t.Errorf("got (%v, %q), want the zero result reported", res, reason)
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
// (consumerServer); here a local channel drives it directly.
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
// tests consumerServer.Stop (it owns the subscriptions' stop budget, not client.Shutdown; ADR keel-0005, "Only the
// cleanup function closes the connection") by constructing a consumerServer literal directly: it is the real type
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

	srv := &consumerServer{
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

// ---- Real lifecycle: an in-process real NATS+JetStream server, public API only. ----

func mustPublisher(t *testing.T, c *Client, streams ...Stream) Publisher {
	t.Helper()
	p, err := NewPublisher(c, streams...)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	return p
}

func testConfig(url string) *kconfig.Config {
	return &kconfig.Config{Config: &conf.Config{
		App:  &conf.App{Domain: "keel", Name: "test"},
		Nats: map[string]*conf.NATS{"default": {Url: url}},
	}}
}

// startConsumerServer starts srv.Start in a background goroutine and waits until the NATS server has every durable.
//
// The wait is necessary: a new durable receives only messages published after its creation (DeliverNewPolicy), so a
// message published before the durable exists is permanently invisible to it. Start creates the durables and then
// blocks, so the only signal is the server's own state, polled with a deadline.
func startConsumerServer(t *testing.T, srv transport.Server) {
	t.Helper()
	go func() { _ = srv.Start(context.Background()) }()
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for _, cs := range srv.(*consumerServer).consumers {
		for {
			if _, err := cs.c.JetStream().Consumer(ctx, cs.stream, cs.opt.Durable); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("durable %s not created on the server within 5s", cs.opt.Durable)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestPublishSubscribe(t *testing.T) {
	c, cleanup, err := NewClient(testConfig(startTestNATS(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cleanup()
	p := mustPublisher(t, c, NewStream("t"))

	received := make(chan string, 4)
	srv, err := NewConsumerServer(c, func(r ConsumerRegistrar) {
		r.Register(func(ctx context.Context, d *Delivery) HandlerResult {
			received <- d.Topic
			return OK()
		}, ConsumerOptions{Stream: NewStream("t"), Durable: "d"})
	})
	if err != nil {
		t.Fatalf("NewConsumerServer: %v", err)
	}
	startConsumerServer(t, srv)

	if err := p.Publish(context.Background(), NewStream("t"), "created", "hello"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case topic := <-received:
		if topic != "created" {
			t.Errorf("topic = %q, want created", topic)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("message not received")
	}
}

// Retry→give up: the handler asks to retry twice in a row and gives up on the third delivery; the backoff table
// decides how long until the message is delivered again (1s, 3s), and after giving up there is no 4th delivery.
func TestRetryThenGiveUp(t *testing.T) {
	c, cleanup, err := NewClient(testConfig(startTestNATS(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cleanup()
	p := mustPublisher(t, c, NewStream("t"))

	var mu sync.Mutex
	var attempts []int
	delivered := make(chan struct{}, 8)

	srv, err := NewConsumerServer(c, func(r ConsumerRegistrar) {
		r.Register(func(ctx context.Context, d *Delivery) HandlerResult {
			mu.Lock()
			attempts = append(attempts, d.RetryAttempts)
			n := len(attempts)
			mu.Unlock()
			delivered <- struct{}{}
			if n < 3 {
				return RetryLater(errors.New("try again"))
			}
			return Failed(errors.New("cannot process"))
		}, ConsumerOptions{Stream: NewStream("t"), Durable: "d"})
	})
	if err != nil {
		t.Fatalf("NewConsumerServer: %v", err)
	}
	startConsumerServer(t, srv)

	if err := p.Publish(context.Background(), NewStream("t"), "created", "x"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Deliveries 1, 2 and 3: backoff table 1s, 3s, plus some margin.
	for i := 0; i < 3; i++ {
		select {
		case <-delivered:
		case <-time.After(8 * time.Second):
			t.Fatalf("too few deliveries, got only %d", i)
		}
	}

	// After giving up there is no 4th delivery (the next redelivery would come after 10s, so confirming none within 3s
	// is enough to prove the "give up" took effect, rather than just landing before the next backoff round).
	select {
	case <-delivered:
		t.Fatal("no redelivery expected after giving up")
	case <-time.After(3 * time.Second):
	}

	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 3 || attempts[0] != 0 || attempts[1] != 1 || attempts[2] != 2 {
		t.Errorf("RetryAttempts sequence = %v, want [0 1 2]", attempts)
	}
}

// Shutdown order: consumerServer.Stop stops only its own subscriptions and the connection stays usable; the connection
// is closed only by the cleanup function NewClient returns (ADR keel-0005, "Only the cleanup function closes the
// connection").
func TestStopOrder_ConnectionOutlivesConsumerServerStop(t *testing.T) {
	c, cleanup, err := NewClient(testConfig(startTestNATS(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cleanup()
	p := mustPublisher(t, c, NewStream("t"))

	if c.Conn().IsClosed() {
		t.Fatal("the connection must not be closed right after it is established")
	}

	received := make(chan string, 4)
	srv, err := NewConsumerServer(c, func(r ConsumerRegistrar) {
		r.Register(func(ctx context.Context, d *Delivery) HandlerResult {
			received <- d.Topic
			return OK()
		}, ConsumerOptions{Stream: NewStream("t"), Durable: "d"})
	})
	if err != nil {
		t.Fatalf("NewConsumerServer: %v", err)
	}
	startConsumerServer(t, srv)

	if err := p.Publish(context.Background(), NewStream("t"), "ping", "1"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("the first publish was not consumed")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if c.Conn().IsClosed() {
		t.Fatal("consumerServer.Stop must not close the connection")
	}
	if err := p.Publish(context.Background(), NewStream("t"), "ping", "2"); err != nil {
		t.Fatalf("publishing should still work after the subscriptions stop: %v", err)
	}

	cleanup()
	if !c.Conn().IsClosed() {
		t.Error("only NewClient's cleanup function closes the connection, so it must be closed after cleanup")
	}
	if err := p.Publish(context.Background(), NewStream("t"), "ping", "3"); !errors.Is(err, ErrClientClosed) {
		t.Errorf("Publish after cleanup should return ErrClientClosed, got %v", err)
	}
}

// Registration errors are returned together: this confirms it also holds on the "real connection" path
// (TestConsumerRegistration_ValidationErrors covers construction without a connection). Checks happen before any
// network I/O, so a bad config is found without actually starting a consumer.
func TestNewConsumerServer_RegistrationErrorsWithRealConnection(t *testing.T) {
	c, cleanup, err := NewClient(testConfig(startTestNATS(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cleanup()

	if _, err := NewConsumerServer(c, func(r ConsumerRegistrar) {
		r.Register(okHandler, ConsumerOptions{Stream: NewStream("layout"), Durable: "notify", Subject: "keel.layout.todo.created"})
	}); !errors.Is(err, ErrSubjectPrefixed) {
		t.Errorf("got %v, want ErrSubjectPrefixed", err)
	}
}

// ---- Publisher: streams are checked at creation, and it publishes only to the streams given. ----

// The stream exists but does not receive the subjects under this stream's name, so published events reach nobody.
// Such a config error must fail when the publisher is built, so the publishing process fails to start; reported only
// on the first publish, the error would be swallowed by a caller that "only logs publish failures".
func TestNewPublisher_FailsWhenStreamDoesNotCoverSubjects(t *testing.T) {
	c, cleanup, err := NewClient(testConfig(startTestNATS(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cleanup()

	if _, err := c.JetStream().CreateStream(context.Background(), jetstream.StreamConfig{
		Name:     "keel_t",
		Subjects: []string{"elsewhere.>"},
	}); err != nil {
		t.Fatalf("pre-create stream: %v", err)
	}

	if _, err := NewPublisher(c, NewStream("t")); err == nil {
		t.Fatal("NewPublisher should return an error when the stream does not receive keel.t.>")
	}
}

// Two streams given at once, one of them twice: both can be published to, each landing in its own stream.
// Deduplicating the list must not swallow another stream, and publishing must not leak onto another stream's subject.
func TestPublisher_MultipleStreams(t *testing.T) {
	c, cleanup, err := NewClient(testConfig(startTestNATS(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cleanup()

	todo, order := NewStream("todo"), NewStream("order")
	p := mustPublisher(t, c, todo, todo, order)
	ctx := context.Background()
	if err := p.Publish(ctx, todo, "created", "x"); err != nil {
		t.Fatalf("publish todo: %v", err)
	}
	for range 2 {
		if err := p.Publish(ctx, order, "paid", "x"); err != nil {
			t.Fatalf("publish order: %v", err)
		}
	}

	for name, want := range map[string]uint64{"keel_todo": 1, "keel_order": 2} {
		s, err := c.JetStream().Stream(ctx, name)
		if err != nil {
			t.Fatalf("stream %s: %v", name, err)
		}
		if got := s.CachedInfo().State.Msgs; got != want {
			t.Errorf("stream %s msgs = %d, want %d", name, got, want)
		}
	}
}

// A stream missing from the list: publishing to it must fail on the spot and name the stream, instead of quietly
// creating the stream and publishing; otherwise the list would be optional and the startup check meaningless.
func TestPublisher_RejectsUndeclaredStream(t *testing.T) {
	c, cleanup, err := NewClient(testConfig(startTestNATS(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cleanup()

	if _, err := NewPublisher(c); !errors.Is(err, ErrNoStreams) {
		t.Errorf("empty list: got %v, want ErrNoStreams", err)
	}

	p := mustPublisher(t, c, NewStream("t"))
	err = p.Publish(context.Background(), NewStream("order"), "created", "x")
	if !errors.Is(err, ErrStreamNotDeclared) || !strings.Contains(err.Error(), `"order"`) {
		t.Errorf("got %v, want ErrStreamNotDeclared naming \"order\"", err)
	}
}

// ---- Event contracts: publishing and subscribing start from the same EventType. ----

type testEventPayload struct {
	Msg string
}

// Contract publish → contract subscribe works end to end: Handle's fn receives the decoded payload and DeliveryInfo
// directly, without the handler calling AsJson itself.
func TestEventContract_PublishSubscribe(t *testing.T) {
	c, cleanup, err := NewClient(testConfig(startTestNATS(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cleanup()
	p := mustPublisher(t, c, NewStream("t"))

	ev := NewEventType[testEventPayload](NewStream("t"), "created")
	type got struct {
		payload testEventPayload
		info    DeliveryInfo
	}
	received := make(chan got, 4)
	srv, err := NewConsumerServer(c, func(r ConsumerRegistrar) {
		ev.Handle(r, func(ctx context.Context, p testEventPayload, d DeliveryInfo) HandlerResult {
			received <- got{payload: p, info: d}
			return OK()
		}, EventOptions{Durable: "notify"})
	})
	if err != nil {
		t.Fatalf("NewConsumerServer: %v", err)
	}
	startConsumerServer(t, srv)

	if err := ev.Publish(context.Background(), p, testEventPayload{Msg: "hello"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case g := <-received:
		if g.payload.Msg != "hello" {
			t.Errorf("payload = %+v, want Msg=hello", g.payload)
		}
		if g.info.RetryAttempts != 0 {
			t.Errorf("RetryAttempts = %d, want 0", g.info.RetryAttempts)
		}
		if g.info.Sequence == 0 {
			t.Error("Sequence should be the in-stream sequence number, not the zero value")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event not received")
	}
}

// An undecodable payload is given up directly: the framework stops it before fn, so fn never sees a bad payload. It
// bypasses EventType.Publish (which cannot encode bad JSON) and publishes invalid JSON as bytes to simulate the input.
func TestEventContract_UndecodablePayloadDropped(t *testing.T) {
	c, cleanup, err := NewClient(testConfig(startTestNATS(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cleanup()
	p := mustPublisher(t, c, NewStream("t"))

	ev := NewEventType[testEventPayload](NewStream("t"), "created")
	called := make(chan struct{}, 1)
	srv, err := NewConsumerServer(c, func(r ConsumerRegistrar) {
		ev.Handle(r, func(ctx context.Context, p testEventPayload, d DeliveryInfo) HandlerResult {
			called <- struct{}{}
			return OK()
		}, EventOptions{Durable: "notify"})
	})
	if err != nil {
		t.Fatalf("NewConsumerServer: %v", err)
	}
	startConsumerServer(t, srv)

	if err := p.PublishEx(context.Background(), NewStream("t"), "created", Publishing{Body: []byte("not-json")}); err != nil {
		t.Fatalf("publish malformed payload: %v", err)
	}

	select {
	case <-called:
		t.Fatal("an undecodable payload must not reach the handler")
	case <-time.After(2 * time.Second):
	}
}

// A byte-level wildcard subscription receives several subjects: one handler receives a set of business subjects
// through a Subject wildcard and branches on Topic inside. Contracts do not cover this case; it uses the byte-level
// entry kept as a supported capability.
func TestByteLevelWildcardSubscribe_ReceivesMultipleSubjects(t *testing.T) {
	c, cleanup, err := NewClient(testConfig(startTestNATS(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cleanup()
	p := mustPublisher(t, c, NewStream("t"))

	received := make(chan string, 4)
	srv, err := NewConsumerServer(c, func(r ConsumerRegistrar) {
		r.Register(func(ctx context.Context, d *Delivery) HandlerResult {
			received <- d.Topic
			return OK()
		}, ConsumerOptions{Stream: NewStream("t"), Subject: "todo.>", Durable: "notify"})
	})
	if err != nil {
		t.Fatalf("NewConsumerServer: %v", err)
	}
	startConsumerServer(t, srv)

	for _, subject := range []string{"todo.created", "todo.done"} {
		if err := p.Publish(context.Background(), NewStream("t"), subject, subject); err != nil {
			t.Fatalf("publish %s: %v", subject, err)
		}
	}

	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case topic := <-received:
			got[topic] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("received only %d", i)
		}
	}
	if !got["todo.created"] || !got["todo.done"] {
		t.Errorf("the wildcard subscription should receive both subjects, got %v", got)
	}
}

// A message the handler keeps retrying is given up after MaxDeliver deliveries; a message the handler fails is given
// up at once. Both land in DeadLetterStream with consumer, original sequence and reason, and replaying one delivers
// the original message again.
func TestDeadLetters(t *testing.T) {
	c, cleanup, err := NewClient(testConfig(startTestNATS(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cleanup()
	p := mustPublisher(t, c, NewStream("dl"))

	var retried, failed atomic.Int32
	srv, err := NewConsumerServer(c, func(r ConsumerRegistrar) {
		r.Register(func(ctx context.Context, d *Delivery) HandlerResult {
			retried.Add(1)
			return RetryLater(errors.New("try again"))
		}, ConsumerOptions{Stream: NewStream("dl"), Subject: "retry", Durable: "retrier", MaxDeliver: 2})
		r.Register(func(ctx context.Context, d *Delivery) HandlerResult {
			failed.Add(1)
			return Failed(errors.New("cannot process"))
		}, ConsumerOptions{Stream: NewStream("dl"), Subject: "fail", Durable: "failer"})
	})
	if err != nil {
		t.Fatalf("NewConsumerServer: %v", err)
	}
	startConsumerServer(t, srv)

	ctx := context.Background()
	if err := p.Publish(ctx, NewStream("dl"), "retry", map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if err := p.Publish(ctx, NewStream("dl"), "fail", map[string]int{"n": 2}); err != nil {
		t.Fatal(err)
	}

	var dls []DeadLetter
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if dls, err = ListDeadLetters(ctx, c.JetStream()); err == nil && len(dls) == 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(dls) != 2 {
		t.Fatalf("got %d dead letters (err %v), want 2", len(dls), err)
	}
	if got := retried.Load(); got != 2 {
		t.Errorf("retrying handler ran %d times, want MaxDeliver=2", got)
	}
	byConsumer := map[string]DeadLetter{}
	for _, dl := range dls {
		byConsumer[dl.Consumer] = dl
	}
	if dl := byConsumer["retrier"]; !strings.Contains(dl.Reason, "no deliveries left") || !strings.Contains(dl.Reason, "try again") || dl.Stream != "keel_dl" {
		t.Errorf("retrier dead letter = %+v, want the handler's error and deliveries in the reason, stream keel_dl", dl)
	}
	if dl := byConsumer["failer"]; dl.Reason != "handler gave up: cannot process" || dl.Deliveries != 1 {
		t.Errorf("failer dead letter = %+v, want reason %q after 1 delivery", dl, "handler gave up: cannot process")
	}

	if err := ReplayDeadLetter(ctx, c.JetStream(), byConsumer["failer"].Seq); err != nil {
		t.Fatalf("ReplayDeadLetter: %v", err)
	}
	for time.Now().Before(deadline) && failed.Load() < 2 {
		time.Sleep(50 * time.Millisecond)
	}
	if got := failed.Load(); got != 2 {
		t.Errorf("failing handler ran %d times, want 2 (original + replay)", got)
	}
}

// Replaying an advisory whose original message is past retention says so instead of failing obscurely.
func TestReplayDeadLetter_OriginalGone(t *testing.T) {
	c, cleanup, err := NewClient(testConfig(startTestNATS(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cleanup()
	ctx := context.Background()
	js := c.JetStream()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "orig", Subjects: []string{"orig.>"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: DeadLetterStream, Subjects: []string{"fake.advisory"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish(ctx, "fake.advisory", []byte(`{"stream":"orig","consumer":"x","stream_seq":42}`)); err != nil {
		t.Fatal(err)
	}
	if err := ReplayDeadLetter(ctx, js, 1); !errors.Is(err, ErrOriginalGone) {
		t.Errorf("got %v, want ErrOriginalGone", err)
	}
}
