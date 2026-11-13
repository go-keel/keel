package knats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/go-keel/keel/core/internal/msgtrace"
	"github.com/go-keel/keel/core/kconfig"
	"github.com/go-keel/keel/internal/errs"
	"github.com/go-keel/keel/pkg/json"
)

const (
	// defaultStreamMaxAge is the default retention of a stream knats creates; overridden by
	// nats.<name>.stream_max_age. It decides how long events can be looked up, and how far back a subscriber other
	// than a new durable can reach when replaying by hand.
	defaultStreamMaxAge = 7 * 24 * time.Hour
	// defaultDeadLetterMaxAge is the retention of the DEAD_LETTER stream when knats creates it; overridden by
	// nats.<name>.dead_letter_max_age.
	defaultDeadLetterMaxAge = 30 * 24 * time.Hour

	// defaultMaxProcessTime is the default processing time limit for one message; overridden by
	// ConsumerOptions.MaxProcessTime.
	defaultMaxProcessTime = 10 * time.Second
	// defaultMaxDeliver is ConsumerOptions.MaxDeliver's default: the first delivery plus 3 retries.
	defaultMaxDeliver = 4
	// ackWaitFactor scales the processing time limit up to the server-side ack timeout (10s → 30s).
	//
	// It cannot be 1: the prefetch window is Workers messages, and a buffered message's clock starts the moment it is
	// **delivered**, yet it waits for the message the worker is processing to finish before its own turn, so two rounds
	// are its real limit. 3 leaves 50% margin above that floor, absorbing variance in handler time and the scheduling
	// and network cost of the Ack itself.
	//
	// A larger window would break this factor: with a window of 2*Workers, the refill happens when the buffer runs dry
	// while the worker still holds one message, the new batch queues behind it, and the last message waits three
	// rounds, exactly filling 3 with zero margin.
	ackWaitFactor = 3

	// drainTimeout is the fallback limit when no stop budget is given, used in two places: (1) closing the
	// connection: the cleanup function NewClient returns uses it when it builds its own ctx, and keeping it within 10
	// seconds is deliberate, as the cleanup phase should not spend much budget waiting for one connection to drain;
	// (2) the fallback in gracefulStop.run when the caller gives no deadline (see stopBudget). kratos always hands down
	// a deadline through app.stop_timeout, so this path is reached only in tests or manual calls.
	drainTimeout = 10 * time.Second

	defaultWorkers = 1

	connectTimeout = 5 * time.Second
	opTimeout      = 10 * time.Second
)

// NewClient connects using the "default" section of the nats config.
func NewClient(c *kconfig.Config) (*Client, func(), error) {
	return NewNamedClient(c, "default")
}

// NewNamedClient connects using the nats.<name> section.
func NewNamedClient(c *kconfig.Config, name string) (*Client, func(), error) {
	if name == "" {
		name = "default"
	}
	sc := c.Nats[name]
	if sc == nil {
		return nil, nil, errs.Rules.New("knats.config-missing", fmt.Sprintf("the config has no nats.%s section", name), fmt.Sprintf("add `nats: {%s: {url: ...}}` to the config or fix the name passed to NewNamedClient", name))
	}

	clientName := c.App.GetName()
	if id := c.App.GetId(); id != "" {
		clientName += "-" + id
	}
	return newClientForURL(c.App.GetDomain(), clientName, sc.Url, sc.CredsFile, streamOptions{
		MaxAge:           sc.GetStreamMaxAge().AsDuration(),
		Replicas:         int(sc.GetStreamReplicas()),
		DeadLetterMaxAge: sc.GetDeadLetterMaxAge().AsDuration(),
	})
}

// streamOptions is the config knats uses when it **creates** a stream. An existing stream is adopted as it is and
// these values do not apply to it; see ensureStream. Unexported: it is only read from the nats.<name> section.
type streamOptions struct {
	// MaxAge is the retention; 0 means the default of 7 days.
	MaxAge time.Duration
	// Replicas is the replica count; 0 means 1. A clustered deployment must set it to match the cluster: it is an
	// availability parameter, and too small a value loses data when a broker fails.
	Replicas int
	// DeadLetterMaxAge is the retention of the DEAD_LETTER stream, 0 means 30 days.
	DeadLetterMaxAge time.Duration
}

// normalized fills in defaults: 0 and unset mean "use the default". Negative values are rejected by the rules on
// conf.proto when the config is loaded.
func (o streamOptions) normalized() streamOptions {
	if o.MaxAge == 0 {
		o.MaxAge = defaultStreamMaxAge
	}
	if o.Replicas == 0 {
		o.Replicas = 1
	}
	if o.DeadLetterMaxAge == 0 {
		o.DeadLetterMaxAge = defaultDeadLetterMaxAge
	}
	return o
}

// newClientForURL connects directly without reading the config. Unexported: the only entries are NewClient /
// NewNamedClient, and cases that bring their own connection details use the Conn() escape hatch.
func newClientForURL(domain, clientName, url, credsFile string, so streamOptions) (*Client, func(), error) {
	if domain == "" {
		return nil, nil, errs.Rules.New("knats.domain-missing", "app.domain is empty, so knats cannot build stream names and subject prefixes", "set app.domain in the config")
	}
	c := newClient(domain, clientName, so.normalized())
	// The only reliable signal that the connection reached CLOSED: Drain is asynchronous, and a background timeout
	// also ends up here.
	notifyClosed := sync.OnceFunc(func() { close(c.connClosed) })

	opts := []nats.Option{
		nats.Name(clientName),
		nats.ClosedHandler(func(*nats.Conn) { notifyClosed() }),
		nats.Timeout(connectTimeout),
		// Reconnecting is left to the client itself: MaxReconnects(-1) means retry forever, with no need to write
		// reconnection and topology redeclaration by hand as with AMQP.
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			slog.Warn("nats disconnected", "client", clientName, "error", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			slog.Info("nats reconnected", "client", clientName, "url", nc.ConnectedUrl())
		}),
	}
	if credsFile != "" {
		opts = append(opts, nats.UserCredentials(credsFile))
	}

	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, err
	}

	c.nc, c.js = nc, js

	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
		defer cancel()
		_ = c.close(ctx)
	}
	return c, cleanup, nil
}

// newClient builds only the internal state; newClientForURL fills in the connection. **Every channel is initialised
// here**: missing one ends in a close of nil channel or, worse, a nil channel with select/default silently swallowing
// reports. Tests use this constructor too.
func newClient(domain, clientName string, so streamOptions) *Client {
	return &Client{
		domain: domain, clientName: clientName, streamOpts: so,
		closed:     make(chan struct{}),
		connClosed: make(chan struct{}),
		stopper:    newGracefulStop(),
	}
}

// Client is a NATS JetStream connection. It cannot publish events itself: events go through the publisher built by
// NewPublisher and subscriptions through NewConsumerServer. Tests replace publishing through the Publisher interface,
// not the Client (ADR keel-0005).
//
// The connection is closed only by the cleanup function NewClient / NewNamedClient return; Client has no public close
// method. The subscriptions' lifetime belongs to the keel.Server returned by NewConsumerServer; the two do not
// interfere, and the connection keeps working after the subscriptions stop.
//
// It holds no subscription state: subscriptions (the consumer list, asynchronous death reports) belong to
// consumerServer, whose Stop stops only them.
type Client struct {
	domain     string
	clientName string
	streamOpts streamOptions
	nc         *nats.Conn
	js         jetstream.JetStream

	// connClosed is closed once the NATS connection has really reached CLOSED. Conn.Drain() only starts the drain in
	// the background and returns (`go nc.drainConnection()` in nats.go v1.53.1 nats.go); leaving without waiting for
	// it loses the Acks not yet flushed along with the process, and those messages are redelivered later.
	connClosed chan struct{}

	closed  chan struct{}
	stopper gracefulStop
}

// Conn returns the underlying NATS connection and JetStream the underlying JetStream context, for users calling native
// capabilities knats does not wrap (core NATS request-reply, KV, Object Store, custom stream configs, publishing to
// streams whose name is unknown at startup and so on).
//
// **Calls through these two exits lose knats's protections**: no domain/stream prefix, no trace context injection, no
// backoff retries, no startup stream checks. For those, use NewPublisher / NewConsumerServer.
func (c *Client) Conn() *nats.Conn               { return c.nc }
func (c *Client) JetStream() jetstream.JetStream { return c.js }

// close closes the connection. It is unexported: its only caller is the cleanup function returned by newClientForURL,
// and the public surface deliberately offers no second way to close (ADR keel-0005, "Only the cleanup function closes
// the connection").
//
// When ctx expires it stops waiting and returns ctx's error instead of hanging here indefinitely; drainTimeout already
// keeps this budget within 10 seconds.
func (c *Client) close(ctx context.Context) error {
	if c.stopper.run(ctx, func() { close(c.closed) }, c.shutdown) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("knats: shutdown gave up waiting; some messages fall back to redelivery")
}

// shutdown is the actual stop flow, started in the background by close.
func (c *Client) shutdown(ctx context.Context) {
	if c.nc == nil {
		return
	}
	// Drain lets in-flight messages (including Acks not yet flushed) finish before disconnecting. It only starts the
	// work in the background and returns, so we must wait for the connection to really close; otherwise the process
	// may exit first, the Acks are lost and those messages are redelivered. The background drain's own timeout is
	// visible only here too.
	if err := c.nc.Drain(); err != nil {
		c.nc.Close()
		return
	}
	select {
	case <-c.connClosed:
	case <-ctx.Done():
		slog.Warn("nats connection drain cut short; unflushed acks fall back to redelivery",
			"client", c.clientName)
		c.stopper.abandoned.Store(true)
		c.nc.Close()
	}
}

// streamName maps a business name to a stream name. NATS stream names may not contain `.`, so the parts are joined
// with an underscore, distinct from the dotted hierarchy of subjects.
func (c *Client) streamName(name string) string {
	return c.domain + "_" + name
}

// subjectPrefix is the subject prefix owned by one stream. Each stream receives only messages under its own prefix,
// so several streams never compete for the same subjects.
func (c *Client) subjectPrefix(name string) string {
	return c.domain + "." + name
}

// ensureStream sets up a stream: it creates it only if it does not exist, and adopts an existing one as it is.
//
// It deliberately avoids CreateOrUpdateStream, which internally does Update then Create, pushing the config here to
// the server and overwriting the retention, replicas and storage backend set by operations. Shortening retention
// deletes messages on the spot, and a different storage type fails outright; both gamble with production data.
//
// So ownership is: **whoever creates it decides**. A stream the framework creates follows streamOptions; a stream
// operations created in advance stays as it is, with only a log line when the config differs. The cost is that the
// framework never adds subjects to an existing stream: widening the wildcard of ConsumerOptions.Subject needs a manual
// update by operations.
func (c *Client) ensureStream(ctx context.Context, name string, subjects []string) error {
	return c.ensureStreamAge(ctx, name, subjects, c.streamOpts.MaxAge)
}

// ensureStreamAge is ensureStream with an explicit retention for a newly created stream.
func (c *Client) ensureStreamAge(ctx context.Context, name string, subjects []string, maxAge time.Duration) error {
	s, err := c.js.Stream(ctx, name)
	if err == nil {
		return c.adopt(ctx, name, subjects, s.CachedInfo().Config, maxAge)
	}
	if !errors.Is(err, jetstream.ErrStreamNotFound) {
		return err
	}

	// The storage backend is fixed to file: unlike replicas it does not affect availability, and memory streams are a
	// very narrow case.
	_, err = c.js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      name,
		Subjects:  subjects,
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.FileStorage,
		Replicas:  c.streamOpts.Replicas,
		MaxAge:    maxAge,
	})
	if err == nil || !errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
		return err
	}
	// Two instances starting for the first time together both see NotFound. With identical configs the server is
	// idempotent (addStreamWithAssignment in nats-server 2.14.5 stream.go takes the reflect.DeepEqual branch and
	// returns the existing stream), so reaching here means the two configs diverge; a diverging config is exactly
	// what adopt tolerates, and it should not become a hard failure just because of a startup race.
	s, err = c.js.Stream(ctx, name)
	if err != nil {
		return err
	}
	return c.adopt(ctx, name, subjects, s.CachedInfo().Config, maxAge)
}

// adopt takes over an existing stream: it checks only the preconditions the framework needs to run and touches no
// other config.
func (c *Client) adopt(ctx context.Context, name string, subjects []string, cfg jetstream.StreamConfig, maxAge time.Duration) error {
	// If the subjects are not covered, messages are not received, and silently so, which is far harder to track down
	// than a startup failure.
	for _, want := range subjects {
		if !subjectCovered(cfg.Subjects, want) {
			return errs.Rules.New("knats.stream-subjects",
				fmt.Sprintf("stream %q exists but does not cover subject %q (has %v)", name, want, cfg.Subjects),
				"add the subject to the stream with the nats CLI, or delete the stream and let knats recreate it")
		}
	}
	// Retention and replicas do not affect whether it runs, only how long events can be looked up and whether a
	// broker failure loses data. The stream's current state wins, with a log line so people know the config did not
	// take effect.
	if cfg.MaxAge > 0 && cfg.MaxAge < maxAge {
		slog.WarnContext(ctx, "nats stream retention is shorter than configured; the stream wins",
			"stream", name, "stream_max_age", cfg.MaxAge, "config", maxAge)
	}
	if cfg.Replicas != c.streamOpts.Replicas {
		slog.WarnContext(ctx, "nats stream replicas differ from config; the stream wins",
			"stream", name, "stream_replicas", cfg.Replicas, "config", c.streamOpts.Replicas)
	}
	return nil
}

// subjectCovered reports whether want falls within one of the existing subject patterns.
//
// Only two kinds of coverage count: identical, or an existing pattern ending in `>` with want under its prefix
// (`keel.>` covers `keel.layout.>`). The single-token wildcard `*` is not considered: deriving coverage precisely with
// it is easy to get wrong, and the wrong direction is "thought covered, actually receives nothing".
func subjectCovered(have []string, want string) bool {
	for _, h := range have {
		if h == want {
			return true
		}
		if prefix, ok := strings.CutSuffix(h, ">"); ok && strings.HasPrefix(want, prefix) {
			return true
		}
	}
	return false
}

// NewPublisher builds a publisher that can publish only to streams, checking each one at build time: a missing stream
// is created, an existing one is checked to receive every subject under this stream's name. If any is wrong (including
// unreachable, or lacking permission to look up or create streams) it returns an error: the publishing process fails
// to start, instead of failing on the first publish and being swallowed by a caller that "only logs publish failures"
// (ADR keel-0005, "The publisher receives all stream contracts at creation").
//
// It does not actually publish: permission to publish to the subjects is not checked, so publishing can still fail at
// runtime.
//
// A stream given twice is checked once.
func NewPublisher(cl *Client, streams ...Stream) (Publisher, error) {
	if len(streams) == 0 {
		return nil, errs.Rules.Wrap(ErrNoStreams, "knats.no-streams", "NewPublisher was called with no streams", "pass every stream this process publishes to, such as NewPublisher(client, OrderStream)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	p := &publisher{c: cl, streams: make(map[Stream]struct{}, len(streams))}
	for _, s := range streams {
		if _, dup := p.streams[s]; dup {
			continue
		}
		if err := s.validate(); err != nil {
			return nil, err
		}
		if err := cl.ensureStream(ctx, cl.streamName(s.name), []string{cl.subjectPrefix(s.name) + ".>"}); err != nil {
			return nil, fmt.Errorf("knats: stream %q: %w", s.name, err)
		}
		p.streams[s] = struct{}{}
	}
	return p, nil
}

// publisher accepts only the streams checked at creation. The publish path neither creates nor checks streams: the
// listed streams were already checked, and a stream deleted or changed at runtime is reported on the spot by
// JetStream's publish acknowledgement.
type publisher struct {
	c       *Client
	streams map[Stream]struct{}
}

func (p *publisher) Publish(ctx context.Context, stream Stream, subject string, v any) error {
	body, err := json.MarshalToBytes(v)
	if err != nil {
		return err
	}
	return p.PublishEx(ctx, stream, subject, Publishing{ContentType: "application/json", Body: body})
}

func (p *publisher) PublishEx(ctx context.Context, stream Stream, subject string, msg Publishing) error {
	if _, ok := p.streams[stream]; !ok {
		return errs.Rules.Wrap(ErrStreamNotDeclared, "knats.stream-undeclared", fmt.Sprintf("Publish to stream %q, which was not passed to NewPublisher", stream.name), "add the stream to the NewPublisher call that built this Publisher")
	}
	if subject == "" {
		return errs.Rules.Wrap(ErrEmptySubject, "knats.subject-empty", fmt.Sprintf("Publish to stream %q with an empty subject", stream.name), "pass the business topic as the subject, such as \"order.paid\"")
	}
	c := p.c
	prefix := c.subjectPrefix(stream.name)
	// With the prefix included the message goes to a subject nobody subscribes to, vanishing without an error.
	if err := checkSubjectPrefix(subject, prefix); err != nil {
		return err
	}
	select {
	case <-c.closed:
		return ErrClientClosed
	default:
	}

	full := prefix + "." + subject
	m := &nats.Msg{Subject: full, Data: msg.Body, Header: nats.Header{}}
	for k, v := range msg.Header {
		m.Header.Set(k, v)
	}
	if msg.ContentType != "" {
		m.Header.Set(headerContentType, msg.ContentType)
	}

	// StartSend starts a span and writes the trace context into m.Header, from which StartReceive in consumer.process
	// continues the trace.
	ctx, span := msgtrace.StartSend(ctx, "nats", "nats.publish", full, headerCarrier(m.Header))
	defer span.End()

	// Publish waits synchronously for the server ack; there is no need for AMQP-style DeferredConfirmation.
	if _, err := c.js.PublishMsg(ctx, m); err != nil {
		msgtrace.MarkFailed(span, err)
		return err
	}
	return nil
}

const headerContentType = "Content-Type"

// headerCarrier makes nats.Header satisfy propagation.TextMapCarrier.
type headerCarrier nats.Header

func (h headerCarrier) Get(key string) string { return nats.Header(h).Get(key) }

func (h headerCarrier) Set(key, value string) { nats.Header(h).Set(key, value) }

func (h headerCarrier) Keys() []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	return keys
}

// trimPrefix removes the framework prefix from a subject, restoring the business subject passed when publishing.
func trimPrefix(subject, prefix string) string {
	return strings.TrimPrefix(strings.TrimPrefix(subject, prefix), ".")
}
