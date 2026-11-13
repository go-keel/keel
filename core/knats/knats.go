// Package knats provides real-time event publishing and subscribing on NATS JetStream.
//
// Model: one stream collects one kind of event, and several durables each consume the same messages independently; the
// publisher publishes once and does not know how many subscribers there are.
//
// **Real-time broadcast only**: no delayed delivery (NakWithDelay holds MaxAckPending quota while waiting; message
// scheduling needs a per-message schedule and an extra stream). An action with a definite execution time is a task,
// not an event: core/kasynq in the stack, a workflow engine for long processes. Messages a consumer gives up on are
// recorded as server advisories in DeadLetterStream; see deadletter.go.
package knats

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-keel/keel/internal/errs"
	"github.com/go-keel/keel/pkg/json"
)

var (
	ErrClientClosed     = errors.New("nats client is closed")
	ErrNilHandler       = errors.New("consumer handler is nil")
	ErrEmptyStream      = errors.New("stream name is empty")
	ErrNoConsumers      = errors.New("no consumer registered")
	ErrEmptySubject     = errors.New("subject is empty")
	ErrEmptyDurable     = errors.New("consumer durable name is empty")
	ErrDuplicateDurable = errors.New("duplicate consumer durable name")
	// ErrSubjectPrefixed means the subject repeats the framework prefix. The resulting subject is syntactically valid,
	// can be published to and can get a consumer, but the two sides never match: only the framework knows this part
	// is not the user's to write; NATS cannot tell.
	ErrSubjectPrefixed = errors.New("subject must not include the framework prefix")
	// ErrStreamNameDotted means the business stream name contains a dot. streamName() builds domain_<name> from it, so
	// a dotted name yields a dotted NATS stream name, which nats-server explicitly rejects (see the source cited in
	// the checkStreamName comment). It is rejected here so the error happens when the publisher is built or the
	// consumer registered, instead of as a server error after ensureStream has sent its network request.
	ErrStreamNameDotted = errors.New("stream name must not contain '.'")
	// ErrNegativeOption means an option is negative. 0 and unset mean "use the default"; a negative value is written
	// but has no effect, a "wrong but silent" case, so it is rejected on the spot.
	ErrNegativeOption = errors.New("option must not be negative")
	// ErrNoStreams means NewPublisher received no stream contract at all, the same meaning as ErrNoConsumers: a
	// publisher that accepts no destination cannot publish any event once built.
	ErrNoStreams = errors.New("no stream declared to publisher")
	// ErrStreamNotDeclared means an event was published to a stream not given when the publisher was created. A
	// missing entry only happens when a whole new stream is added; the error carries the stream name, so one glance
	// tells which entry to add in di.go.
	ErrStreamNotDeclared = errors.New("stream not declared to publisher")
)

// Stream is a stream contract: the definition of one stream, which holds several event contracts, usually one per
// business domain.
//
// It carries only the stream name: retention and replicas come from nats.<name>.stream_max_age / stream_replicas, and
// an existing stream is kept as it is (see ensureStream). The stream is written as a package-level variable that the
// event contracts and NewPublisher both reference, so the name is written once in the whole repo. All streams share
// one type: a misspelled variable name does not compile, but referencing another existing stream is not caught by the
// compiler.
type Stream struct {
	name string
}

// NewStream defines a stream. Whether the name is valid (non-empty, no dot) is checked only when NewPublisher or
// consumer registration gets a Client: it is used in package-level variables, where there is nowhere to return an
// error.
func NewStream(name string) Stream {
	return Stream{name: name}
}

// validate matches how streamName() builds the name: an empty name yields no stream, a dotted one yields a stream
// name NATS rejects.
func (s Stream) validate() error {
	if s.name == "" {
		return errs.Rules.Wrap(ErrEmptyStream, "knats.stream-empty", "a Stream has an empty name", "give NewStream a non-empty name, such as knats.NewStream(\"order\")")
	}
	return checkStreamName(s.name)
}

// EventType binds an event's stream contract, subject and payload type in one definition. It is the recommended entry
// for publishing and subscribing, shaped like kasynq.TaskType:
//
//	var StreamTodo = knats.NewStream("todo")
//	var TodoCreated = knats.NewEventType[TodoCreatedPayload](StreamTodo, "created")
//	TodoCreated.Publish(ctx, publisher, payload)                            // publish
//	TodoCreated.Handle(r, myHandler, knats.EventOptions{Durable: "notify"}) // subscribe
//
// The stream and subject are written in one place, instead of as unrelated bare strings in a publisher file and a
// subscriber file where a typo on one side silently receives nothing. A misspelled variable name does not compile, nor
// does a mismatched payload type; referencing another contract with the same payload type is not caught by the
// compiler. It takes over JSON encoding and decoding on both sides. Custom encoding, subjects built at runtime and
// wildcard subscriptions use the byte-level entries Publisher.PublishEx / ConsumerRegistrar.Register, which are a
// supported public capability, not internal plumbing.
type EventType[T any] struct {
	stream  Stream
	subject string
}

// NewEventType binds an event contract. Whether the subject is valid (no framework prefix) is checked only when
// Publish / Handle actually get a Client: the contract itself does not know the domain and cannot tell.
func NewEventType[T any](stream Stream, subject string) EventType[T] {
	return EventType[T]{stream: stream, subject: subject}
}

// Publish encodes the payload and publishes it to the contract's stream and subject.
//
// It takes the Publisher interface: a business test that replaces the publishing layer only needs a hand-written
// double implementing two methods (ADR keel-0005, "Rejected approaches").
func (e EventType[T]) Publish(ctx context.Context, p Publisher, v T) error {
	return p.Publish(ctx, e.stream, e.subject, v)
}

// EventHandler handles the decoded payload of a contract event; its return value means the same as ConsumerHandler's.
//
// It receives read-only delivery info instead of the full Delivery: the payload is already decoded into the payload
// argument, the topic is fixed by the contract's subject, and the raw body means nothing at this layer. Needing
// byte-level details such as the topic or headers is the byte-level subscription case: use ConsumerHandler +
// Delivery.
type EventHandler[T any] func(ctx context.Context, payload T, d DeliveryInfo) HandlerResult

// DeliveryInfo is the read-only information delivered to an event contract's handler.
type DeliveryInfo struct {
	// RetryAttempts is the number of redeliveries so far, 0 on the first delivery; business code uses it to decide when
	// to give up (Failed) instead of asking for another retry.
	RetryAttempts int
	// Sequence is the message's sequence number in the stream, monotonically increasing and unchanged on redelivery,
	// usable as a deduplication key (fmt.Sprintf("order-settle-%d", d.Sequence)).
	Sequence uint64
}

// EventOptions holds the tuning options of a contract subscription. Stream/Subject are already given by the contract
// and are not taken again here: a value the caller filled in would be overridden by the contract anyway, so the trap
// is not offered at all.
type EventOptions struct {
	// Durable: see ConsumerOptions.Durable.
	Durable string
	// MaxProcessTime: see ConsumerOptions.MaxProcessTime; 0 means the same default.
	MaxProcessTime time.Duration
	// Workers: see ConsumerOptions.Workers; 0 means the same default.
	Workers int
	// MaxDeliver: see ConsumerOptions.MaxDeliver; 0 means the same default.
	MaxDeliver int
}

// Handle registers a strongly typed handler.
//
// A decode failure gives up directly (Failed with the decode error) without calling fn: retrying cannot decode a bad
// payload, so the message is terminated and the decode error is the reason in the log and the dead letter advisory.
func (e EventType[T]) Handle(r ConsumerRegistrar, fn EventHandler[T], opt EventOptions) {
	r.Register(func(ctx context.Context, d *Delivery) HandlerResult {
		var p T
		if err := d.AsJson(&p); err != nil {
			return Failed(fmt.Errorf("decode %s payload: %w", e.subject, err))
		}
		return fn(ctx, p, DeliveryInfo{RetryAttempts: d.RetryAttempts, Sequence: d.Sequence})
	}, ConsumerOptions{
		Stream:         e.stream,
		Subject:        e.subject,
		Durable:        opt.Durable,
		MaxProcessTime: opt.MaxProcessTime,
		Workers:        opt.Workers,
		MaxDeliver:     opt.MaxDeliver,
	})
}

// Publisher is a process's only way to publish events. It is created by NewPublisher and can publish only to the
// stream contracts given at creation.
//
// It is an interface rather than a struct for business tests: a double implements just these two methods, without
// also implementing Client's Conn()/JetStream() escape hatches (ADR keel-0005, "Rejected approaches").
type Publisher interface {
	// Publish serialises v to JSON and publishes it to subject under stream. A stream not in the list given at
	// creation returns ErrStreamNotDeclared.
	Publish(ctx context.Context, stream Stream, subject string, v any) error
	// PublishEx publishes an already built message, for cases that need custom headers or a non-JSON encoding.
	PublishEx(ctx context.Context, stream Stream, subject string, msg Publishing) error
}

// Publishing is a message to publish.
type Publishing struct {
	ContentType string
	Body        []byte
	// Header holds extra custom headers; the framework also injects the trace context. Keys must not start with
	// Nats-.
	Header map[string]string
}

// Delivery is the message delivered to a byte-level subscription handler. An event contract's handler receives the
// decoded payload + DeliveryInfo instead of this struct: the contract need not expose the topic again (the contract
// gives it) nor the raw bytes (already decoded).
type Delivery struct {
	// Topic is the business subject without the framework prefix, equal to the subject passed when publishing.
	Topic string
	Body  []byte
	// RetryAttempts is the number of redeliveries so far, 0 on the first delivery.
	//
	// It comes from JetStream's native NumDelivered; Keel keeps no counter header of its own.
	RetryAttempts int
	Header        map[string]string
	// Sequence is the message's sequence number in the stream, monotonically increasing and unchanged on redelivery,
	// usable as a deduplication key.
	//
	// It comes from JetStream's native Metadata().Sequence.Stream. The raw message object (jetstream.Msg) is not
	// exposed: the framework handles ack/nak/term, and the only reasonable thing business code could take from the
	// raw object is the sequence number; once the number is given, the whole object is not needed.
	Sequence uint64
}

// AsJson unmarshals the message body into v.
func (d *Delivery) AsJson(v any) error {
	return json.UnmarshalBytes(d.Body, v)
}

// HandlerResult is a handler's disposition of one message, built only with OK, RetryLater or Failed. The error given
// to RetryLater and Failed becomes the reason in the give-up log and the dead letter advisory, so an operator replaying
// a dead letter sees why it was given up without searching the logs.
type HandlerResult struct {
	kind resultKind
	err  error
}

type resultKind int

const (
	// resultInvalid is the zero HandlerResult, which no constructor returns; it is retried like RetryLater.
	resultInvalid resultKind = iota
	resultOK
	resultRetryLater
	resultFailed
)

// OK: processed successfully; the message is acknowledged.
func OK() HandlerResult { return HandlerResult{kind: resultOK} }

// RetryLater: processing failed, redeliver later per the backoff table. Capped by ConsumerOptions.MaxDeliver: when the
// last delivery still asks to retry, the message is given up like Failed, with err as the reason.
func RetryLater(err error) HandlerResult { return HandlerResult{kind: resultRetryLater, err: err} }

// Failed: processing failed and retrying will not help; the message is terminated with err as the reason. The server
// then publishes a give-up advisory that lands in DeadLetterStream; the message itself stays in its stream until
// retention ends (see ListDeadLetters / ReplayDeadLetter).
func Failed(err error) HandlerResult { return HandlerResult{kind: resultFailed, err: err} }

// String names the result the way it was built, with its error: "OK", "RetryLater: <err>" or "Failed: <err>". Tests of
// a handler compare it, since two results built from different error values are not ==.
func (r HandlerResult) String() string {
	name := map[resultKind]string{resultOK: "OK", resultRetryLater: "RetryLater", resultFailed: "Failed"}[r.kind]
	switch {
	case r.kind == resultInvalid:
		return "invalid zero HandlerResult"
	case r.err == nil || r.kind == resultOK:
		return name
	}
	return name + ": " + r.err.Error()
}

// reason is the text written to the give-up log and the dead letter advisory.
func (r HandlerResult) reason(fallback string) string {
	if r.err == nil {
		return fallback + " (no error given)"
	}
	return fallback + ": " + r.err.Error()
}

// ConsumerHandler handles one message and returns its disposition.
//
// It is a function type rather than an interface: a handler is pure behaviour, and a single-method interface would only
// force a matching wrapper constructor. An implementer using a struct method passes the method value (obj.Handle).
type ConsumerHandler func(ctx context.Context, d *Delivery) HandlerResult

// ConsumerSetup registers all subscriptions at once when the consumer server is built, shaped like kasynq's
// WorkerSetup / SchedulerSetup: it returns no error. Validation failures (duplicate durable, invalid subject, negative
// option and so on) are recorded on the ConsumerRegistrar and returned together by NewConsumerServer after setup
// runs.
type ConsumerSetup func(r ConsumerRegistrar)

// ConsumerRegistrar is the registration interface exposed to ConsumerSetup.
type ConsumerRegistrar interface {
	// Register registers a consumer. A validation failure is neither returned here nor a panic; it is recorded on the
	// registrar and returned together by NewConsumerServer.
	Register(h ConsumerHandler, opt ConsumerOptions)
}

// ConsumerOptions describes a durable consumer.
type ConsumerOptions struct {
	// Stream is the stream to subscribe to, the same variable as the stream contract used for publishing.
	Stream Stream
	// Subject is the business subject and supports the NATS wildcards * and >. Empty subscribes to every message of
	// the stream.
	//
	// It is the part **without the framework prefix**; including the prefix builds a syntactically valid subject that
	// nobody publishes to, so registration rejects it with ErrSubjectPrefixed.
	Subject string
	// Durable is the durable consumer name, unique within a stream. It decides who owns the consumption progress.
	Durable string
	// MaxProcessTime is the processing time limit for one message; 0 means the default of 10 seconds.
	//
	// It is **a deadline for the handler**: the handler's ctx is Done when it expires and downstream calls fail with
	// it. The framework **does not forcibly interrupt** the handler (Go cannot), so a handler that ignores ctx and keeps
	// running still gets its message Acked normally, without redelivery, as long as it returns before the server-side
	// ack timeout.
	//
	// The real redelivery threshold is 3 times this value (AckWait, see ackWaitFactor): of those 3 shares one is for
	// buffering and queueing, one is margin, and the handler gets the 1 share here. So "write the handler to
	// MaxProcessTime" both leaves yourself a safety cushion and is the reason this value exists.
	//
	// **All instances of a durable must configure the same value**: it decides the server-side AckWait, and
	// CreateOrUpdateConsumer writes it on every start, so the last instance to start wins. If one instance configures
	// 10 seconds and another 60, the latter's handler is redelivered before its own deadline.
	//
	// If something really needs to run longer, rather than raising this, enqueue the slow work as a task
	// (core/kasynq): the event model is "handle on receipt and be done".
	MaxProcessTime time.Duration
	// Workers is the number of goroutines processing messages concurrently; 0 means the default of 1. It also decides
	// this instance's prefetch window (Workers messages) and is the only knob here that affects consumption
	// concurrency.
	//
	// It applies only to this instance's client and is not written to the server-side consumer config, so several
	// instances of one durable **need not** agree (MaxProcessTime is the one that must).
	//
	// Raising it gives up ordering: with several workers processing at once, an earlier message does not necessarily
	// finish first. And when a durable is deployed as several instances, messages are spread across them, so even
	// Workers=1 does not keep order then. The event model is at-least-once delivery; handlers must be idempotent
	// anyway.
	Workers int
	// MaxDeliver caps deliveries of one message, the first delivery included; 0 means 4 (first delivery plus 3
	// retries). It is written to the server-side consumer config, so all instances of a durable must agree.
	MaxDeliver int
}
