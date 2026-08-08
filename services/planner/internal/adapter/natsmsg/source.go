// Package natsmsg binds the projector to the durable pull consumers created by
// deploy/nats/init.sh.
package natsmsg

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/mhayk/overpass/services/planner/internal/app"
	"github.com/mhayk/overpass/services/planner/internal/port"
)

// Source is a MessageSource over the planner's two bound pull subscriptions.
//
// The ack handles live here, keyed by stream and sequence, rather than riding
// inside port.Message as an opaque `any`. The port describes what the
// application needs; making it carry a *nats.Msg would put the transport into
// the interface that exists to keep the transport out.
type Source struct {
	subs  map[string]*nats.Subscription
	order []string
	batch int
	wait  time.Duration

	mu       sync.Mutex
	inflight map[string]*nats.Msg
}

func handleKey(m port.Message) string {
	return m.Stream + "/" + strconv.FormatUint(m.Sequence, 10)
}

// Bind attaches to the pre-declared durable consumers.
//
// Bind, not create. The topology is declared once in deploy/nats/init.sh and
// versioned there; a service that creates its own consumer on start silently
// diverges from it the first time a setting changes. Both consumers this binds
// — planner-opportunities and planner-lifecycle — have existed since M0-06 with
// nothing attached to them.
func Bind(js nats.JetStreamContext, batch int, wait time.Duration) (*Source, error) {
	s := &Source{
		subs:     make(map[string]*nats.Subscription, len(app.Streams)),
		batch:    batch,
		wait:     wait,
		inflight: make(map[string]*nats.Msg),
	}
	for _, binding := range app.Streams {
		// The subject passed to PullSubscribe must EQUAL the consumer's
		// declared filter. Passing "" — which reads like "whatever the consumer
		// already filters on" — fails with "nats: subject does not match
		// consumer", as does nats.BindStream with an empty subject. So ask the
		// broker rather than duplicating the filter here: a copy of
		// "tasking.request.>" in this file is a copy that drifts from
		// deploy/nats/init.sh the first time the topology changes.
		info, err := js.ConsumerInfo(binding.Stream, binding.Consumer)
		if err != nil {
			return nil, fmt.Errorf("looking up %s/%s: %w", binding.Stream, binding.Consumer, err)
		}
		subject := info.Config.FilterSubject
		if subject == "" && len(info.Config.FilterSubjects) > 0 {
			subject = info.Config.FilterSubjects[0]
		}

		sub, err := js.PullSubscribe(subject, binding.Consumer, nats.Bind(binding.Stream, binding.Consumer))
		if err != nil {
			return nil, fmt.Errorf("binding %s/%s on %q: %w", binding.Stream, binding.Consumer, subject, err)
		}
		s.subs[binding.Stream] = sub
		s.order = append(s.order, binding.Stream)
	}
	return s, nil
}

// Next drains one batch from the first stream that has anything.
func (s *Source) Next(ctx context.Context) ([]port.Message, error) {
	for _, stream := range s.order {
		// A derived context rather than MaxWait alongside Context: the client
		// rejects both together with "context and timeout can not both be set",
		// and dropping either loses something that matters — MaxWait alone
		// ignores shutdown, Context alone lets one fetch block on a quiet
		// stream while the other starves.
		fetchCtx, cancel := context.WithTimeout(ctx, s.wait)
		msgs, err := s.subs[stream].Fetch(s.batch, nats.Context(fetchCtx))
		cancel()

		// A derived deadline surfaces as DeadlineExceeded, not ErrTimeout.
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			continue // an idle stream is the normal case, not a failure
		}
		switch {
		case errors.Is(err, nats.ErrTimeout):
			continue
		case err != nil:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("fetching from %s: %w", stream, err)
		}

		out := make([]port.Message, 0, len(msgs))
		s.mu.Lock()
		for _, m := range msgs {
			converted, convErr := convert(stream, m)
			if convErr != nil {
				// Metadata that will not parse will not parse on redelivery
				// either. Term rather than Nak, so it stops occupying a
				// delivery slot forever.
				_ = m.Term() //nolint:errcheck // nothing useful to do if the term itself fails
				continue
			}
			s.inflight[handleKey(converted)] = m
			out = append(out, converted)
		}
		s.mu.Unlock()
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
		Stream:   stream,
		Sequence: meta.Sequence.Stream,
		Subject:  m.Subject,
		EventID:  m.Header.Get("Nats-Msg-Id"),
		Payload:  m.Data,
		EventAt:  eventAt(m.Header, meta.Timestamp),
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
		// projection as decades behind, which is far worse than a few seconds
		// of skew.
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
