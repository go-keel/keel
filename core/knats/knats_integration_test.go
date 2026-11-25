//go:build integration

// In-package: drives the client, publishers and consumers against the real NATS server from TEST_NATS_URL.

package knats

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
	"github.com/go-keel/keel/internal/integrationtest"
	"github.com/go-keel/keel/pkg/rule"
)

// startTestNATS returns the real server from TEST_NATS_URL after deleting every stream on it, so each test starts
// from an empty server, and deletes them again when the test ends.
func startTestNATS(t *testing.T) string {
	t.Helper()
	url := integrationtest.Env(t, "TEST_NATS_URL")
	deleteAllStreams(t, url)
	// Leave the server empty too: other tools (a smoke run, a worker started by hand) share it, and a
	// stream left behind with test subjects fails their startup check.
	t.Cleanup(func() { deleteAllStreams(t, url) })
	return url
}

// deleteAllStreams deletes every stream on the server at url.
func deleteAllStreams(t *testing.T, url string) {
	t.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect %s: %v", url, err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	names := js.StreamNames(ctx)
	var all []string
	for n := range names.Name() {
		all = append(all, n)
	}
	if err := names.Err(); err != nil {
		t.Fatal(err)
	}
	for _, n := range all {
		if err := js.DeleteStream(ctx, n); err != nil {
			t.Fatalf("delete stream %s: %v", n, err)
		}
	}
}

// ---- Real lifecycle: a real NATS+JetStream server, public API only. ----

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
func startConsumerServer(t *testing.T, srv *ConsumerServer) {
	t.Helper()
	go func() { _ = srv.Start(context.Background()) }()
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for _, cs := range srv.consumers {
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

// Shutdown order: ConsumerServer.Stop stops only its own subscriptions and the connection stays usable; the connection
// is closed only by the cleanup function NewClient returns.
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
		t.Fatal("ConsumerServer.Stop must not close the connection")
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

// A stream that cannot be created because another stream in the account already captures its subjects fails with a
// rule error that says how to find and fix the other stream, not the bare JetStream API error.
func TestEnsureStreamSubjectsOverlap(t *testing.T) {
	c, cleanup, err := NewClient(testConfig(startTestNATS(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cleanup()
	ctx := context.Background()
	if err := c.ensureStream(ctx, "overlap_first", []string{"overlap.>"}); err != nil {
		t.Fatalf("first stream: %v", err)
	}
	err = c.ensureStream(ctx, "overlap_second", []string{"overlap.x"})
	var re *rule.Error
	if !errors.As(err, &re) || re.ID != "knats.stream-subjects-overlap" {
		t.Fatalf("overlapping stream: err = %v, want knats.stream-subjects-overlap", err)
	}
}
