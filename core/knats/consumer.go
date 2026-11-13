package knats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"

	"github.com/go-keel/keel/core/internal/msgtrace"
	"github.com/go-keel/keel/internal/errs"
)

// consumerRegistrar collects the Register calls in ConsumerSetup and records validation failures in err, the same rule
// as mw.Chain recording an error on the chain when an operation by name fails; NewConsumerServer takes it out and
// returns it after setup runs.
type consumerRegistrar struct {
	c         *Client
	consumers []*consumer
	err       error
}

func (r *consumerRegistrar) Register(h ConsumerHandler, opt ConsumerOptions) {
	if h == nil {
		r.err = errors.Join(r.err, errs.Rules.Wrap(ErrNilHandler, "knats.handler-nil", fmt.Sprintf("Register for durable %q was given a nil handler", opt.Durable), "pass a non-nil ConsumerHandler to Register"))
		return
	}
	// The prefix is computed with subjectPrefix, the same source used to build the filter. When opt.Stream is empty,
	// validate returns ErrEmptyStream first and the step using it is never reached.
	if err := opt.validate(r.c.subjectPrefix(opt.Stream.name)); err != nil {
		r.err = errors.Join(r.err, err)
		return
	}

	if opt.Workers == 0 {
		opt.Workers = defaultWorkers
	}
	if opt.MaxProcessTime == 0 {
		opt.MaxProcessTime = defaultMaxProcessTime
	}
	if opt.MaxDeliver == 0 {
		opt.MaxDeliver = defaultMaxDeliver
	}

	prefix := r.c.subjectPrefix(opt.Stream.name)
	filter := prefix + ".>"
	if opt.Subject != "" {
		filter = prefix + "." + opt.Subject
	}
	stream := r.c.streamName(opt.Stream.name)

	// A duplicate durable on the same stream makes two handlers compete for one consumption progress, each getting
	// half of the messages. The same name on different streams is legal.
	for _, exist := range r.consumers {
		if exist.opt.Durable == opt.Durable && exist.stream == stream {
			r.err = errors.Join(r.err, errs.Rules.Wrap(ErrDuplicateDurable, "knats.durable-duplicate", fmt.Sprintf("durable %q is registered twice on stream %q", opt.Durable, stream), "give each consumer on the same stream its own Durable name"))
			return
		}
	}
	r.consumers = append(r.consumers, &consumer{
		c: r.c, opt: opt, handler: h, stream: stream, filter: filter,
		jobs:    make(chan jetstream.Msg),
		done:    make(chan struct{}),
		stopper: newGracefulStop(),
	})
}

type consumer struct {
	c       *Client
	opt     ConsumerOptions
	handler ConsumerHandler

	// stream / filter are where this consumer actually subscribes, derived from opt and stored once.
	stream string
	filter string

	jobs chan jetstream.Msg
	wg   sync.WaitGroup

	// mu guards cc and stopped. kratos runs Start and Stop for each server in two concurrent goroutines, so a stop
	// signal arriving during startup collides: stop() reads cc while start() is writing it.
	mu      sync.Mutex
	cc      jetstream.ConsumeContext
	stopped bool

	done    chan struct{}
	stopper gracefulStop
}

// adoptConsumeContext takes the subscription just created. If stop() has already run, nobody else can close this
// subscription (stopOnce is used up), so it is closed on the spot.
func (cs *consumer) adoptConsumeContext(cc jetstream.ConsumeContext) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.stopped {
		cc.Stop()
		return
	}
	cs.cc = cc
}

// start creates the subscription. failures is passed in by consumerServer, which owns it (it is not on client, which
// manages only the connection); watch uses it to report asynchronous death.
func (cs *consumer) start(ctx context.Context, failures chan error) error {
	initCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	// Consumers starting before publishers is a normal deployment order, so a missing stream is created here.
	prefix := cs.c.subjectPrefix(cs.opt.Stream.name)
	if err := cs.c.ensureStream(initCtx, cs.stream, []string{prefix + ".>"}); err != nil {
		return err
	}
	// Give-up advisories are only kept if the dead letter stream exists before they are published.
	if err := cs.c.ensureStreamAge(initCtx, DeadLetterStream, deadLetterSubjects, cs.c.streamOpts.DeadLetterMaxAge); err != nil {
		return err
	}

	stream, err := cs.c.js.Stream(initCtx, cs.stream)
	if err != nil {
		return err
	}
	cfg := jetstream.ConsumerConfig{
		Durable:       cs.opt.Durable,
		FilterSubject: cs.filter,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ackWaitFactor * cs.opt.MaxProcessTime,
		// MaxAckPending stays 0, leaving it to operations' layered control: the stream's ConsumerLimits → server/account
		// limits → a fallback of 1000 (setConsumerConfigDefaults in nats-server 2.14.5 consumer.go). A value forced in
		// by the framework would override the stream-level ConsumerLimits, and an explicit value above the server
		// limit makes consumer creation fail outright rather than being narrowed. It is not responsible for timeout
		// safety either; PullMaxMessages decides that, see below.
		MaxDeliver: cs.opt.MaxDeliver,
	}

	// A new durable receives only messages published after its creation. The upstream default is DeliverAllPolicy
	// ("This is the default" in nats.go v1.53.1 jetstream/consumer_config.go), which means that when a new subscriber
	// comes online, every historical event within retention is replayed: every notification sent again, every point
	// added again.
	//
	// An existing durable keeps its current policy untouched: DeliverPolicy and the matching OptStartSeq/OptStartTime
	// are among the fields nats-server refuses to update (checkNewConsumerConfig, "deliver policy can not be
	// updated"), so forcing a new value would make the first start after an upgrade fail outright. Whoever creates it
	// decides, the same rule as for streams.
	switch existing, err := stream.Consumer(initCtx, cs.opt.Durable); {
	case err == nil:
		old := existing.CachedInfo().Config
		cfg.DeliverPolicy = old.DeliverPolicy
		cfg.OptStartSeq, cfg.OptStartTime = old.OptStartSeq, old.OptStartTime
	case errors.Is(err, jetstream.ErrConsumerNotFound):
		cfg.DeliverPolicy = jetstream.DeliverNewPolicy
	default:
		return err
	}

	jsCons, err := stream.CreateOrUpdateConsumer(initCtx, cfg)
	if err != nil {
		return err
	}

	for range cs.opt.Workers {
		cs.wg.Go(func() { cs.work(ctx) })
	}

	// The Consume callback is serial, so it only hands messages over here; the real processing runs concurrently in
	// the workers.
	//
	// The prefetch window is derived from the concurrency: it is how many messages **one instance** buffers, a
	// different thing from the server's durable-level total quota.
	//
	// It must be given explicitly: upstream prefetches 500 messages by default (nats.go v1.53.1
	// jetstream_options.go:138), and messages prefetched to the client already count as delivered on the server, with
	// their clock running. With Workers messages, a buffered message only waits for the one the worker holds to finish
	// before its turn, capped at two rounds, which is what gives ackWaitFactor's 3 its margin.
	//
	// Throughput does not suffer: the refill happens the moment a worker takes a message and overlaps with its
	// processing, so there is no idle gap as long as one round of processing takes longer than one pull round trip.
	cc, err := jsCons.Consume(func(msg jetstream.Msg) {
		select {
		case cs.jobs <- msg:
		case <-cs.done:
			// Messages received while stopping are handed back to the server for redelivery, not swallowed.
			_ = msg.Nak()
		}
	},
		jetstream.PullMaxMessages(cs.opt.Workers),
		// Most errors here are recoverable (a missed heartbeat re-sends the pull automatically), so they are only
		// logged; the truly fatal signal is the subscription being closed, which the watchdog below watches for.
		jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, err error) {
			slog.WarnContext(ctx, "nats consume error",
				"error", err, "durable", cs.opt.Durable, "stream", cs.stream)
		}),
	)
	if err != nil {
		_ = cs.stop(initCtx)
		return err
	}
	cs.adoptConsumeContext(cc)
	go cs.watch(cc, failures)
	return nil
}

// watch watches whether the subscription closed by itself without anyone asking it to stop.
//
// On a terminal error (durable deleted, consumer kicked) nats.go Stops the subscription and nothing more happens: the
// process is still alive and health checks still pass, but nobody consumes messages any more. It is reported upward
// so the whole consumer server exits and the orchestrator restarts it.
func (cs *consumer) watch(cc jetstream.ConsumeContext, failures chan error) {
	select {
	case <-cs.done:
		return
	case <-cc.Closed():
	}
	cs.mu.Lock()
	stopped := cs.stopped
	cs.mu.Unlock()
	if stopped {
		return // we stopped it ourselves
	}
	select {
	case failures <- fmt.Errorf("knats: consumer %q on stream %q stopped unexpectedly",
		cs.opt.Durable, cs.stream):
	default: // another consumer already reported; the first one wins
	}
}

// stop stops this consumer and returns whether it stopped cleanly within ctx's budget.
func (cs *consumer) stop(ctx context.Context) bool {
	var cc jetstream.ConsumeContext
	return cs.stopper.run(ctx, func() {
		// stopped must be set immediately: the watchdog uses it to tell "the server kicked us" from "we stopped it
		// ourselves", and one step late would report a false failure.
		cs.mu.Lock()
		cs.stopped = true
		cc, cs.cc = cs.cc, nil
		cs.mu.Unlock()
	}, func(budget context.Context) { cs.shutdown(budget, cc) })
}

// shutdown is the actual stop flow, started in the background by stop with its own budget.
func (cs *consumer) shutdown(ctx context.Context, cc jetstream.ConsumeContext) {
	if cc != nil {
		// Drain rather than Stop: Stop discards the messages in the client buffer outright (nats.go v1.53.1
		// pull.go:60 "already in the buffer are discarded"), and those messages reach another instance only after
		// AckWait expires, and AckWait is scaled up from MaxProcessTime. Drain hands them to the callback to finish.
		//
		// done is closed only after this: closing it earlier sends the callback down the Nak branch, which throws away
		// the buffer ourselves.
		cc.Drain()
		select {
		case <-cc.Closed():
		case <-ctx.Done():
			slog.Warn("nats consumer drain cut short; buffered messages fall back to redelivery",
				"durable", cs.opt.Durable, "stream", cs.stream)
			cs.stopper.abandoned.Store(true)
		}
	}
	close(cs.done)

	// wg.Wait is bounded by ctx as well. When a handler ignores ctx.Done() or a downstream call hangs or deadlocks,
	// waiting without limit stalls the whole process's shutdown until the orchestrator kills it. Giving up the wait
	// leaks a few worker goroutines, but the process is exiting anyway, and the messages they hold are redelivered by
	// the server.
	waited := make(chan struct{})
	go func() { cs.wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-ctx.Done():
		slog.Warn("nats consumer workers did not finish in time; giving up on them",
			"durable", cs.opt.Durable, "stream", cs.stream)
		cs.stopper.abandoned.Store(true)
	}
}

func (cs *consumer) work(ctx context.Context) {
	for {
		select {
		case <-cs.done:
			return
		case <-ctx.Done():
			return
		case msg := <-cs.jobs:
			cs.process(ctx, msg)
		}
	}
}

func (cs *consumer) process(ctx context.Context, msg jetstream.Msg) {
	meta, err := msg.Metadata()
	if err != nil {
		// No metadata means this is not a JetStream message, and redelivery will not make it better.
		slog.ErrorContext(ctx, "nats consumer metadata failed",
			"error", err, "durable", cs.opt.Durable, "subject", msg.Subject())
		_ = msg.Term()
		return
	}

	header := msg.Headers()
	d := &Delivery{
		Topic:         trimPrefix(msg.Subject(), cs.c.subjectPrefix(cs.opt.Stream.name)),
		Body:          msg.Data(),
		RetryAttempts: int(meta.NumDelivered) - 1,
		Sequence:      meta.Sequence.Stream,
	}
	if len(header) > 0 {
		d.Header = make(map[string]string, len(header))
		for k := range header {
			d.Header[k] = header.Get(k)
		}
	}

	// The handler's ctx carries the processing time limit. It is an advisory deadline, not a hard timeout: on expiry it
	// only closes ctx without forcibly interrupting (Go cannot), and a handler that lingers but returns before AckWait
	// (3 times) still gets its message Acked normally. This share exists to give the handler a chance to stop before it
	// is judged timed out: once AckWait is really reached the message is redelivered to someone else, and running on
	// here performs the side effects a second time.
	ctx, cancelProcess := context.WithTimeout(ctx, cs.opt.MaxProcessTime)
	defer cancelProcess()

	// StartReceive continues the trace the producer wrote into the message headers and starts a consumer span; the
	// consumer name (durable) is an event-specific attribute, appended through extra as usual.
	ctx, span := msgtrace.StartReceive(ctx, "nats", "nats.process", msg.Subject(), headerCarrier(header),
		attribute.String("messaging.consumer", cs.opt.Durable))
	defer span.End()

	res, reason := cs.safeHandle(ctx, d)
	switch res.kind {
	case resultOK:
		cs.ack(ctx, msg)
	case resultRetryLater, resultInvalid:
		// Retries and give-ups alike are marked failed; a pending retry is no reason not to mark it.
		msgtrace.MarkFailed(span, errHandlerRetryLater)
		if d.RetryAttempts+1 >= cs.opt.MaxDeliver {
			// Last allowed delivery: terminate with a reason rather than nak, so the dead letter advisory says why.
			cs.giveUp(ctx, msg, d, fmt.Sprintf("%s; no deliveries left (max %d)", reason, cs.opt.MaxDeliver))
			return
		}
		// NakWithDelay replaces AMQP's trio of retry exchange + TTL + dead-letter routing. The backoff is in seconds,
		// so it holds ack-pending only briefly.
		if err := msg.NakWithDelay(retryDelay(d.RetryAttempts)); err != nil {
			slog.ErrorContext(ctx, "nats consumer retry nak failed",
				"error", err, "durable", cs.opt.Durable, "subject", msg.Subject())
		}
	default:
		msgtrace.MarkFailed(span, errHandlerGaveUp)
		cs.giveUp(ctx, msg, d, reason)
	}
}

// giveUp terminates the message with a reason. The server publishes a terminated advisory (stream, consumer, stream
// sequence, deliveries, reason) that DeadLetterStream keeps; the message itself stays in its stream until retention
// ends, which is where ReplayDeadLetter reads it from. process() marks the span failed; this only logs and terminates.
func (cs *consumer) giveUp(ctx context.Context, msg jetstream.Msg, d *Delivery, reason string) {
	slog.ErrorContext(ctx, "nats consumer gave up on message",
		"durable", cs.opt.Durable, "subject", msg.Subject(),
		"attempts", d.RetryAttempts, "reason", reason)
	if err := msg.TermWithReason(reason); err != nil {
		slog.ErrorContext(ctx, "nats consumer term failed",
			"error", err, "durable", cs.opt.Durable, "subject", msg.Subject())
	}
}

func (cs *consumer) ack(ctx context.Context, msg jetstream.Msg) {
	if err := msg.Ack(); err != nil {
		slog.ErrorContext(ctx, "nats consumer ack failed",
			"error", err, "durable", cs.opt.Durable, "subject", msg.Subject())
	}
}

// safeHandle runs the handler and turns a panic into a retry: a panic is not necessarily deterministic (downstream
// timeouts, races), and MaxDeliver bounds how often a poison message comes back. The returned reason ends up in the
// dead letter advisory if the message is given up. Spans are marked in process(), not here.
func (cs *consumer) safeHandle(ctx context.Context, d *Delivery) (res HandlerResult, reason string) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "nats consumer handler panic",
				"panic", r, "durable", cs.opt.Durable, "topic", d.Topic,
				"attempts", d.RetryAttempts, "stack", string(debug.Stack()))
			res, reason = RetryLater(nil), fmt.Sprintf("handler panicked: %v", r)
		}
	}()
	res = cs.handler(ctx, d)
	switch res.kind {
	case resultRetryLater:
		reason = res.reason("handler asked to retry")
	case resultFailed:
		reason = res.reason("handler gave up")
	case resultInvalid:
		reason = "handler returned the zero HandlerResult; return OK(), RetryLater(err) or Failed(err)"
	}
	return res, reason
}

// validate rejects only "written but not in effect" and "wrong but silent"; 0 and unset keep their "use the default"
// meaning.
func (o ConsumerOptions) validate(prefix string) error {
	if o.Durable == "" {
		return errs.Rules.Wrap(ErrEmptyDurable, "knats.durable-empty", fmt.Sprintf("a consumer on stream %q has an empty Durable", o.Stream.name), "set ConsumerOptions.Durable to a name unique on that stream")
	}
	if err := o.Stream.validate(); err != nil {
		return err
	}
	if err := checkSubjectPrefix(o.Subject, prefix); err != nil {
		return err
	}
	if o.Workers < 0 {
		return errs.Rules.Wrap(ErrNegativeOption, "knats.option-negative", fmt.Sprintf("consumer %q has negative Workers %d", o.Durable, o.Workers), "set Workers to a positive number, or 0 for the default")
	}
	if o.MaxProcessTime < 0 {
		return errs.Rules.Wrap(ErrNegativeOption, "knats.option-negative", fmt.Sprintf("consumer %q has negative MaxProcessTime %v", o.Durable, o.MaxProcessTime), "set MaxProcessTime to a positive duration, or 0 for the default")
	}
	if o.MaxDeliver < 0 {
		return errs.Rules.Wrap(ErrNegativeOption, "knats.option-negative", fmt.Sprintf("consumer %q has negative MaxDeliver %d", o.Durable, o.MaxDeliver), "set MaxDeliver to a positive number, or 0 for the default")
	}
	return nil
}

// checkSubjectPrefix rejects a subject that repeats the framework prefix. prefix is <domain>.<stream>.
//
// No other layer reports this error: <domain>.<stream>.<domain>.<stream>.x is syntactically valid, publishing gets into
// the stream and the consumer can be created, but the two sides never match. Only the framework knows this part is not
// the user's to write.
//
// Only the full <domain>.<stream>. prefix counts. Looking at the domain alone would be a false positive: with domain
// keel, the business subject keel.sync builds keel.layout.keel.sync, which still matches on both sides and is legal.
func checkSubjectPrefix(subject, prefix string) error {
	if subject != "" && prefix != "" && strings.HasPrefix(subject, prefix+".") {
		return errs.Rules.Wrap(ErrSubjectPrefixed, "knats.subject-prefixed", fmt.Sprintf("subject %q already starts with the framework prefix %q", subject, prefix+"."), "pass the business topic only; knats adds the <domain>.<stream> prefix itself")
	}
	return nil
}

// checkStreamName rejects a dot in a business stream name. streamName() builds domain_<name> from it, so a dotted name
// yields a dotted NATS stream name, which checkStreamCfg in nats-server 2.14.5 server/stream.go:1649 explicitly
// rejects ("stream name is required and can not contain '.', '*', '>', '\\', '/'"). It is rejected here so the error
// happens when the publisher is built or the consumer registered, instead of as a server error after ensureStream has
// sent its network request.
func checkStreamName(name string) error {
	if strings.Contains(name, ".") {
		return errs.Rules.Wrap(ErrStreamNameDotted, "knats.stream-name", fmt.Sprintf("stream name %q contains '.'", name), "use a stream name without dots, such as \"order_events\"")
	}
	return nil
}

// errHandlerRetryLater / errHandlerGaveUp are used only for msgtrace.MarkFailed: business code never sees them, so they
// need not be exported. ConsumerHandler returns an enum with no error to use, so a descriptive error is added here to
// make the span's failure reason readable (without distinguishing whether a panic caused it: the panic details are
// already in slog, and the span only needs to know "this failed, and of which kind").
var (
	errHandlerRetryLater = errors.New("consumer handler requested retry")
	errHandlerGaveUp     = errors.New("consumer handler gave up")
)

// retryDelay is the redelivery backoff table; the delay grows with the retry count, capped at 60 seconds.
func retryDelay(attempts int) time.Duration {
	switch {
	case attempts <= 0:
		return 1 * time.Second
	case attempts == 1:
		return 3 * time.Second
	case attempts == 2:
		return 10 * time.Second
	case attempts == 3:
		return 30 * time.Second
	default:
		return 60 * time.Second
	}
}
