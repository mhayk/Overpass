// Package natsmsg binds the projector to the durable pull consumers created by
// deploy/nats/init.sh.
package natsmsg

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/mhayk/overpass/lib/go/consume"

	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

// Streams the gateway folds, in the order it drains them.
//
// Tasking first, then feasibility, then planning: that is the causal order, and
// draining it that way means a cold rebuild rarely folds a plan before the
// request it belongs to. It is an optimisation, not a guarantee — the fold is
// order-independent because the broker gives no cross-stream ordering at all.
var Streams = []string{"TASKING", "FEASIBILITY", "PLANNING"}

// Source is a MessageSource over several bound pull subscriptions.
//
// The ack handles live here, in a map keyed by stream and sequence, rather than
// riding along inside port.Message as an opaque `any`. The port describes what
// the application needs; making it carry a *nats.Msg would put the transport
// into the interface that exists to keep the transport out.
type Source struct {
	subs  map[string]*nats.Subscription
	order []string
	batch int
	wait  time.Duration

	// pub and consumerFor are what dead-lettering needs: somewhere to publish,
	// and the name of the consumer that gave up. The application supplies only
	// the reason; the rest of a dead letter is transport detail held here.
	pub         consume.Publisher
	consumerFor map[string]string

	mu       sync.Mutex
	inflight map[string]*nats.Msg
}

// jetstreamPublisher is the transport half of lib/go/consume's Publisher.
//
// It exists because that module imports no NATS, deliberately — the header set
// and the subject mapping are shared, the wire is not.
type jetstreamPublisher struct{ js nats.JetStreamContext }

func (p jetstreamPublisher) Publish(ctx context.Context, subject string, header map[string][]string, payload []byte) error {
	_, err := p.js.PublishMsg(&nats.Msg{
		Subject: subject,
		Header:  nats.Header(header),
		Data:    payload,
	}, nats.Context(ctx))
	return err
}

func handleKey(m port.Message) string {
	return m.Stream + "/" + strconv.FormatUint(m.Sequence, 10)
}

// Bind attaches to the pre-declared durable consumers.
//
// Bind, not create. The topology is declared once in deploy/nats/init.sh and
// versioned there; a service that creates its own consumer on start silently
// diverges from it the first time a setting changes.
func Bind(js nats.JetStreamContext, durablePrefix string, batch int, wait time.Duration) (*Source, error) {
	s := &Source{
		subs:        make(map[string]*nats.Subscription, len(Streams)),
		batch:       batch,
		wait:        wait,
		pub:         jetstreamPublisher{js: js},
		consumerFor: make(map[string]string, len(Streams)),
		inflight:    make(map[string]*nats.Msg),
	}
	for _, stream := range Streams {
		durable := durablePrefix + "-" + strings.ToLower(stream)
		s.consumerFor[stream] = durable

		// The subject passed to PullSubscribe must EQUAL the consumer's
		// declared filter. Measured against a live broker: passing "" — which
		// reads like "whatever the consumer already filters on" — fails with
		// "nats: subject does not match consumer", and so does nats.BindStream
		// with an empty subject. Only the consumer's own FilterSubject works.
		//
		// So ask the broker rather than duplicating the filter here. A copy of
		// "tasking.>" in this file is a copy that drifts from
		// deploy/nats/init.sh the first time the topology changes.
		info, err := js.ConsumerInfo(stream, durable)
		if err != nil {
			return nil, fmt.Errorf("looking up %s/%s: %w", stream, durable, err)
		}
		subject := info.Config.FilterSubject
		if subject == "" && len(info.Config.FilterSubjects) > 0 {
			// A multi-filter consumer reports FilterSubjects and leaves
			// FilterSubject empty. PLANNING is one: planning.> and
			// acquisition.>.
			subject = info.Config.FilterSubjects[0]
		}

		sub, err := js.PullSubscribe(subject, durable, nats.Bind(stream, durable))
		if err != nil {
			return nil, fmt.Errorf("binding %s/%s on %q: %w", stream, durable, subject, err)
		}
		s.subs[stream] = sub
		s.order = append(s.order, stream)
	}
	return s, nil
}

// Next drains one batch from the first stream that has anything.
func (s *Source) Next(ctx context.Context) ([]port.Message, error) {
	for _, stream := range s.order {
		// A derived context rather than MaxWait alongside Context: the client
		// rejects both together with "context and timeout can not both be
		// set", and dropping either one loses something that matters —
		// MaxWait alone ignores shutdown, Context alone lets one fetch block
		// on a quiet stream while the others starve.
		fetchCtx, cancel := context.WithTimeout(ctx, s.wait)
		msgs, err := s.subs[stream].Fetch(s.batch, nats.Context(fetchCtx))
		cancel()

		// A derived deadline surfaces as DeadlineExceeded, not ErrTimeout.
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			continue // an idle stream is the normal case, not a failure
		}
		switch {
		case errors.Is(err, nats.ErrTimeout):
			continue // an idle stream is the normal case, not a failure
		case err != nil:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("fetching from %s: %w", stream, err)
		}
		out := make([]port.Message, 0, len(msgs))
		var unreadable []*nats.Msg
		s.mu.Lock()
		for _, m := range msgs {
			converted, convErr := convert(stream, m)
			if convErr != nil {
				// Metadata that will not parse will not parse on redelivery
				// either, so this ends in a Term — but not before the payload
				// is kept. Collected and handled outside the lock, because
				// dead-lettering publishes and this critical section guards a
				// map, not a network round trip.
				unreadable = append(unreadable, m)
				continue
			}
			s.inflight[handleKey(converted)] = m
			out = append(out, converted)
		}
		s.mu.Unlock()

		for _, m := range unreadable {
			s.deadletterUnreadable(ctx, stream, m)
		}
		return out, nil
	}
	return nil, nil
}

func convert(stream string, m *nats.Msg) (port.Message, error) {
	meta, err := m.Metadata()
	if err != nil {
		return port.Message{}, fmt.Errorf("reading metadata: %w", err)
	}
	return port.Message{
		Stream:    stream,
		Sequence:  meta.Sequence.Stream,
		Subject:   m.Subject,
		EventID:   m.Header.Get("Nats-Msg-Id"),
		Payload:   m.Data,
		EventAt:   eventAt(m.Header, meta.Timestamp),
		Delivered: meta.NumDelivered,
		Headers:   flattenHeaders(m.Header),
	}, nil
}

// eventAt chooses which clock staleness is measured against.
//
// The producer's occurred_at when it sent one, the broker's store time
// otherwise. The broker's timestamp is when the message was STORED, which is
// later than when the thing happened and drifts further the longer the outbox
// backs up — so a busy system would report itself as fresher than it is.
//
// Split out from convert because convert cannot be tested: nats.Msg.Metadata()
// only works on a message bound to a live subscription, so everything around it
// needs a broker. This part is the part with a decision in it.
func eventAt(header nats.Header, brokerAt time.Time) time.Time {
	raw := header.Get("Occurred-At")
	if raw == "" {
		return brokerAt
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		// Fall back rather than propagate. A zero timestamp would report the
		// projection as decades behind and is far worse than a few seconds of
		// skew.
		return brokerAt
	}
	return t
}

// Ack confirms a fold committed.
func (s *Source) Ack(_ context.Context, m port.Message) error {
	raw, err := s.claim(m)
	if err != nil {
		return err
	}
	return raw.Ack()
}

// Nak asks for redelivery.
func (s *Source) Nak(_ context.Context, m port.Message) error {
	raw, err := s.claim(m)
	if err != nil {
		return err
	}
	return raw.Nak()
}

// Term stops delivery on purpose. See port.MessageSource.
func (s *Source) Term(_ context.Context, m port.Message) error {
	raw, err := s.claim(m)
	if err != nil {
		return err
	}
	return raw.Term()
}

// Deadletter publishes the message to its DLQ subject. See port.MessageSource:
// the projector calls this BEFORE Term and Naks if it fails.
//
// It peeks at the in-flight handle rather than claiming it, because the Term
// that follows still needs it. A claim here would make the correct sequence —
// dead-letter, then Term — fail on the Term with "no in-flight handle".
func (s *Source) Deadletter(ctx context.Context, m port.Message, reason string) error {
	raw, err := s.peek(m)
	if err != nil {
		return err
	}
	return consume.Deadletter(ctx, s.pub, consume.DeadLetter{
		Subject:     raw.Subject,
		EventID:     m.EventID,
		Payload:     raw.Data,
		Traceparent: raw.Header.Get(consume.HeaderTraceparent),
		Reason:      reason,
		Delivered:   m.Delivered,
		Consumer:    s.consumerFor[m.Stream],
	})
}

// deadletterUnreadable handles the one message the projector never sees: the
// delivery whose broker metadata would not parse, so it has no sequence and
// cannot be tracked in-flight.
//
// Delivered is 0 and that is not a guess — the delivery count lives in the
// metadata that just failed to parse. The reason header says `metadata`, which
// is what tells an operator the count is missing rather than wrong.
//
// A failed publish Naks: the message comes back and the whole thing is retried,
// same invariant as everywhere else. If it keeps failing the broker's
// max_deliver eventually lapses, which is the one silent drop left in this
// design — it needs both an unparseable delivery AND a broker that will not
// accept a publish.
func (s *Source) deadletterUnreadable(ctx context.Context, stream string, m *nats.Msg) {
	err := consume.Deadletter(ctx, s.pub, consume.DeadLetter{
		Subject:     m.Subject,
		EventID:     m.Header.Get(consume.HeaderMsgID),
		Payload:     m.Data,
		Traceparent: m.Header.Get(consume.HeaderTraceparent),
		Reason:      consume.ReasonMetadata,
		Consumer:    s.consumerFor[stream],
	})
	if err != nil {
		_ = m.Nak() //nolint:errcheck // the redelivery is the retry; a failed Nak is one the ack wait covers
		return
	}
	_ = m.Term() //nolint:errcheck // nothing useful to do if the term itself fails
}

// peek reads the handle without taking it, for the operations that are not the
// last thing to happen to a message.
func (s *Source) peek(m port.Message) (*nats.Msg, error) {
	key := handleKey(m)
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.inflight[key]
	if !ok {
		return nil, fmt.Errorf("no in-flight handle for %s (event %s)", key, m.EventID)
	}
	return raw, nil
}

// claim takes the handle out of the map, so a double ack is a loud error rather
// than a second ack the broker quietly ignores.
func (s *Source) claim(m port.Message) (*nats.Msg, error) {
	key := handleKey(m)
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.inflight[key]
	if !ok {
		return nil, fmt.Errorf("no in-flight handle for %s (event %s)", key, m.EventID)
	}
	delete(s.inflight, key)
	return raw, nil
}

// Drain releases the subscriptions without losing in-flight acks.
func (s *Source) Drain() error {
	var errs []error
	for stream, sub := range s.subs {
		if err := sub.Drain(); err != nil {
			errs = append(errs, fmt.Errorf("draining %s: %w", stream, err))
		}
	}
	return errors.Join(errs...)
}

// flattenHeaders turns NATS's multi-valued header map into the single-valued
// carrier the propagator expects.
//
// First value wins. W3C traceparent is single-valued by specification, and a
// message carrying two would be malformed — taking the first is the same
// choice net/http makes for Header.Get, and guessing between them would be
// worse than being predictable.
//
// nil rather than an empty map when there are no headers, so Extract sees
// "nothing to continue" rather than an empty carrier it has to walk.
func flattenHeaders(header nats.Header) map[string]string {
	if len(header) == 0 {
		return nil
	}
	out := make(map[string]string, len(header))
	for key, values := range header {
		if len(values) > 0 {
			out[key] = values[0]
		}
	}
	return out
}
