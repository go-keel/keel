package knats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// DeadLetterStream is the JetStream stream that keeps the server's advisories about messages a consumer gave up on:
// terminated by the handler (Failed, or out of deliveries) or past MaxDeliver. It is one stream per NATS
// account, shared by every service in it, created by the first consumer server that starts (retention
// nats.<name>.dead_letter_max_age, default 30 days).
//
// It stores advisories, not messages: an entry points at the original message by stream and sequence, and the
// original stays readable only until its own stream's retention (stream_max_age, default 7 days) ends.
const DeadLetterStream = "DEAD_LETTER"

var deadLetterSubjects = []string{
	"$JS.EVENT.ADVISORY.CONSUMER.MSG_TERMINATED.>",
	"$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.>",
}

// ErrOriginalGone is returned by ReplayDeadLetter when the original message is past its stream's retention.
var ErrOriginalGone = errors.New("original message is gone (past its stream's retention)")

// DeadLetter is one give-up advisory.
type DeadLetter struct {
	Seq        uint64    // sequence in DeadLetterStream; the handle for ReplayDeadLetter
	Time       time.Time // when the message was given up
	Stream     string    // stream of the original message
	Consumer   string    // durable that gave up
	StreamSeq  uint64    // sequence of the original message in Stream
	Deliveries uint64
	Reason     string // empty for MAX_DELIVERIES advisories
}

// ListDeadLetters returns every advisory in DeadLetterStream, oldest first.
func ListDeadLetters(ctx context.Context, js jetstream.JetStream) ([]DeadLetter, error) {
	s, err := js.Stream(ctx, DeadLetterStream)
	if err != nil {
		return nil, fmt.Errorf("knats: open %s: %w", DeadLetterStream, err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		return nil, err
	}
	var out []DeadLetter
	for seq := info.State.FirstSeq; info.State.Msgs > 0 && seq <= info.State.LastSeq; seq++ {
		m, err := s.GetMsg(ctx, seq)
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		dl, err := parseDeadLetter(m)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, nil
}

// ReplayDeadLetter republishes the original message of the advisory at seq to its subject. Every consumer of that
// subject receives it again, not only the one that gave up, so handlers must be idempotent (they already must be:
// delivery is at least once). Returns ErrOriginalGone if the original is past retention.
func ReplayDeadLetter(ctx context.Context, js jetstream.JetStream, seq uint64) error {
	s, err := js.Stream(ctx, DeadLetterStream)
	if err != nil {
		return fmt.Errorf("knats: open %s: %w", DeadLetterStream, err)
	}
	m, err := s.GetMsg(ctx, seq)
	if err != nil {
		return fmt.Errorf("knats: dead letter %d: %w", seq, err)
	}
	dl, err := parseDeadLetter(m)
	if err != nil {
		return err
	}
	orig, err := js.Stream(ctx, dl.Stream)
	if err != nil {
		return fmt.Errorf("knats: open stream %s: %w", dl.Stream, err)
	}
	om, err := orig.GetMsg(ctx, dl.StreamSeq)
	if errors.Is(err, jetstream.ErrMsgNotFound) {
		return fmt.Errorf("knats: %s #%d: %w", dl.Stream, dl.StreamSeq, ErrOriginalGone)
	}
	if err != nil {
		return err
	}
	header := nats.Header{}
	for k, v := range om.Header {
		header[k] = v
	}
	// Publish dedup would drop a replay inside the duplicate window if the original id were kept.
	header.Del(jetstream.MsgIDHeader)
	_, err = js.PublishMsg(ctx, &nats.Msg{Subject: om.Subject, Header: header, Data: om.Data})
	return err
}

func parseDeadLetter(m *jetstream.RawStreamMsg) (DeadLetter, error) {
	var a struct {
		Stream     string `json:"stream"`
		Consumer   string `json:"consumer"`
		StreamSeq  uint64 `json:"stream_seq"`
		Deliveries uint64 `json:"deliveries"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal(m.Data, &a); err != nil {
		return DeadLetter{}, fmt.Errorf("knats: dead letter %d is not an advisory: %w", m.Sequence, err)
	}
	return DeadLetter{Seq: m.Sequence, Time: m.Time, Stream: a.Stream, Consumer: a.Consumer, StreamSeq: a.StreamSeq,
		Deliveries: a.Deliveries, Reason: a.Reason}, nil
}
