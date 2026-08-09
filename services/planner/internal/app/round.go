package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mhayk/overpass/services/planner/internal/domain"
	"github.com/mhayk/overpass/services/planner/internal/port"
)

// Trigger sweeps for dirty buckets and opens rounds over the ones that are due.
//
// It does not allocate. M2-01 opens the decision boundary and announces what was
// on the table; M2-04 onward decides what wins. Keeping them apart is what makes
// the serialisation testable on its own — a lock test that also had to produce a
// correct plan would be testing two things and failing for either reason.
type Trigger struct {
	rounds port.Rounds
	policy domain.TriggerPolicy
	log    *slog.Logger

	bucketDuration time.Duration
	horizonAhead   time.Duration
	sweepLimit     int

	// allocator is the AllocationPolicy the round runs. ADR-0007 defers the
	// DEFAULT to the M2-13 benchmark, so which one is configuration, and the
	// round records the name so a committed plan is attributable to a strategy
	// after the fact.
	allocator domain.AllocationPolicy

	// fairness turns snapshots into the effective value policies compete on.
	// It runs HERE, once per round, so no policy ever sees a priority tier.
	fairness domain.Fairness

	// now is injected so the firing rule can be driven to any instant in tests
	// without sleeping. The rule is entirely about clocks; testing it by waiting
	// would be slow and flaky in equal measure.
	now func() time.Time
}

// TriggerConfig is what Trigger needs to run.
type TriggerConfig struct {
	Policy         domain.TriggerPolicy
	BucketDuration time.Duration
	HorizonAhead   time.Duration
	SweepLimit     int
	Allocator      domain.AllocationPolicy
	Fairness       domain.Fairness
	Now            func() time.Time
}

// NewTrigger wires the round trigger, refusing a configuration that cannot
// behave as ADR-0014 describes.
func NewTrigger(rounds port.Rounds, cfg TriggerConfig, log *slog.Logger) (*Trigger, error) {
	if err := cfg.Policy.Validate(); err != nil {
		return nil, err
	}
	if err := domain.ValidBucketDuration(cfg.BucketDuration); err != nil {
		return nil, err
	}
	if cfg.SweepLimit <= 0 {
		return nil, fmt.Errorf("%w: sweep limit must be positive, got %d", domain.ErrInvalid, cfg.SweepLimit)
	}
	if cfg.HorizonAhead <= 0 {
		return nil, fmt.Errorf("%w: horizon must be positive, got %s", domain.ErrInvalid, cfg.HorizonAhead)
	}
	if cfg.Allocator == nil {
		return nil, fmt.Errorf("%w: no allocation policy configured", domain.ErrInvalid)
	}
	if err := cfg.Fairness.Validate(); err != nil {
		return nil, err
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Trigger{
		rounds:         rounds,
		policy:         cfg.Policy,
		log:            log,
		bucketDuration: cfg.BucketDuration,
		horizonAhead:   cfg.HorizonAhead,
		sweepLimit:     cfg.SweepLimit,
		allocator:      cfg.Allocator,
		fairness:       cfg.Fairness,
		now:            now,
	}, nil
}

// SweepStats is what one sweep did.
type SweepStats struct {
	Considered int
	Opened     int
	Waiting    int
	// Skipped counts buckets that were due at sweep time but had nothing left
	// under the lock, because another planner opened a round in between.
	//
	// Counted separately from Opened rather than folded into it. They are
	// different facts — one is work done, the other is a race lost — and
	// conflating them would make "the planner is opening rounds" true of a
	// planner that is opening none.
	Skipped int
	Failed  int
}

// SweepOnce evaluates every dirty bucket in the horizon and opens what is due.
func (t *Trigger) SweepOnce(ctx context.Context) (SweepStats, error) {
	var stats SweepStats
	now := t.now()

	states, err := t.rounds.DirtyBuckets(ctx, port.BucketQuery{
		BucketDuration: t.bucketDuration,
		// From the current bucket's start, not from now: a bucket that has
		// already begun is still partly flyable, and excluding it would drop
		// every candidate in the hour a request was submitted.
		HorizonStart: domain.BucketStart(now, t.bucketDuration),
		HorizonEnd:   now.Add(t.horizonAhead),
		Limit:        t.sweepLimit,
	})
	if err != nil {
		return stats, err
	}
	stats.Considered = len(states)

	for _, state := range states {
		decision := t.policy.Decide(state, now)
		if !decision.Fire {
			stats.Waiting++
			t.log.Debug("bucket not due",
				slog.String("round_key", state.Key.String()),
				slog.String("reason", decision.Reason))
			continue
		}

		// Different satellites are independent, so one failure must not abandon
		// the sweep — that would let a single bad satellite starve the whole
		// constellation.
		opened, err := t.open(ctx, state, decision, now)
		switch {
		case err != nil:
			stats.Failed++
			t.log.Error("opening a round failed",
				slog.String("round_key", state.Key.String()),
				slog.Any("error", err))
		case opened:
			stats.Opened++
		default:
			stats.Skipped++
		}
	}
	return stats, nil
}

func (t *Trigger) open(ctx context.Context, state domain.BucketState, decision domain.Decision, now time.Time) (bool, error) {
	_, bucketEnd := domain.Bucket(state.Key.BucketStart, t.bucketDuration)

	opened, err := t.rounds.OpenRound(ctx, state.Key, bucketEnd,
		func(inputs port.RoundInputs) (port.RoundOutcome, error) {
			// Re-decided under the lock, on what the lock actually saw.
			//
			// Another planner may have opened a round for this bucket between
			// the sweep and the lock, which makes the bucket clean and this
			// round a duplicate of one already recorded. Skipping is not an
			// optimisation: opening anyway would announce a decision boundary
			// over a candidate set that had already been announced, and the
			// conservation ledger would contain the same requests twice.
			if inputs.CandidateOpportunityCount == 0 {
				return port.RoundOutcome{}, port.ErrSkipRound
			}
			// No JOINABLE candidates — everything on the table is held, waiting
			// for its request snapshot. Skipping matters beyond economy: the
			// round would commit an EMPTY plan built on no information, and if
			// a live plan exists, whole-bucket recompute would supersede it
			// with nothing. Held candidates are a fact to wait out, not
			// evidence the bucket emptied.
			if len(inputs.Joinable) == 0 {
				return port.RoundOutcome{}, port.ErrSkipRound
			}

			round := port.Round{
				RoundID:       uuid.NewString(),
				EventID:       uuid.NewString(),
				CorrelationID: uuid.NewString(),
				Key:           inputs.Key,
				BucketEnd:     inputs.BucketEnd,
				Trigger:       decision.Trigger,
				Policy:        t.allocator.Name(),
				// The count is of everything on the table; the ids are only
				// those that can actually compete. See readInputs.
				CandidateOpportunityCount: inputs.CandidateOpportunityCount,
				CandidateRequestIDs:       inputs.CandidateRequestIDs,
				DutyCycleBudgetS:          inputs.DutyCycleBudgetS,
				SupersedesPlanID:          inputs.LivePlanID,
				TriggeredAt:               now,
			}
			// Null when the ceiling fired, the tipping event otherwise. Since
			// REPLAN overloads the trigger field, this is the only thing that
			// separates a cadence-driven replan from a debounce-driven one.
			if decision.CarriesCausation() && state.TippingEventID != "" {
				tipping := state.TippingEventID
				round.CausationID = &tipping
			}

			payload, err := buildRoundTriggeredEvent(round)
			if err != nil {
				return port.RoundOutcome{}, err
			}

			// THE ROUND NOW ALLOCATES. Policy, plan, events — all built here,
			// inside the locked transaction, so what commits is exactly what
			// was decided against the state the lock saw.
			plan, err := t.buildPlan(round, inputs, now, t.log)
			if err != nil {
				return port.RoundOutcome{}, err
			}
			return port.RoundOutcome{Round: round, RoundPayload: payload, Plan: plan}, nil
		})
	if err != nil {
		return false, err
	}

	if !opened {
		t.log.Debug("round skipped under the lock; another planner got there first",
			slog.String("round_key", state.Key.String()))
		return false, nil
	}

	t.log.Info("round opened",
		slog.String("round_key", state.Key.String()),
		slog.String("trigger", decision.Trigger),
		slog.String("reason", decision.Reason))
	return true, nil
}

// Run sweeps until the context is cancelled.
//
// maxIterations exists so a test can run the real loop to completion rather
// than cancelling it — a loop only ever stopped by cancellation is a loop whose
// shutdown path is never exercised.
func (t *Trigger) Run(ctx context.Context, maxIterations int, interval time.Duration) error {
	for i := 0; maxIterations <= 0 || i < maxIterations; i++ {
		stats, err := t.SweepOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil //nolint:nilerr // cancellation is a clean stop
			}
			t.log.Error("sweep failed", slog.Any("error", err))
		} else if stats.Opened > 0 {
			t.log.Info("sweep complete",
				slog.Int("considered", stats.Considered),
				slog.Int("opened", stats.Opened),
				slog.Int("waiting", stats.Waiting),
				slog.Int("failed", stats.Failed))
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
	return nil
}
