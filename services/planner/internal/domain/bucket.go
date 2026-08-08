package domain

import (
	"fmt"
	"hash/fnv"
	"time"
)

// A round is identified by (satellite_id, bucket_start). That pair is the
// advisory-lock key, so allocation for one satellite-bucket is single-writer
// while different satellites plan fully in parallel. The lock granularity
// matches the invariant granularity — the non-overlap constraint is
// per-satellite — which is why serialising the planner does not create a global
// throughput ceiling.

// RoundKey identifies one round.
type RoundKey struct {
	SatelliteID string
	BucketStart time.Time
}

func (k RoundKey) String() string {
	return fmt.Sprintf("%s@%s", k.SatelliteID, k.BucketStart.UTC().Format(time.RFC3339))
}

// ValidBucketDuration reports whether d can produce UTC-aligned buckets.
//
// "Aligned to UTC" is only true if the duration divides a day evenly. A 7-hour
// bucket tiles forward from the Unix epoch and drifts across midnight, so
// "today's 06:00 bucket" would mean different spans on different days and the
// contract's claim that fixed alignment makes rounds reproducible would quietly
// stop holding.
//
// Checked rather than assumed, because the failure is invisible: every
// individual round still works, and only a comparison across days shows the
// drift.
func ValidBucketDuration(d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("%w: bucket duration must be positive, got %s", ErrInvalid, d)
	}
	if 24*time.Hour%d != 0 {
		return fmt.Errorf("%w: bucket duration %s does not divide 24h evenly, so buckets cannot be UTC-aligned", ErrInvalid, d)
	}
	return nil
}

// BucketStart floors an instant onto its bucket boundary, in UTC.
//
// Fixed alignment rather than a rolling window, per the contract: a rolling
// window makes the same input produce different partitions depending on when it
// ran, and a round that cannot be replayed onto the same partition cannot be
// investigated.
//
// time.Truncate is measured from the zero time, not from the epoch, but the two
// agree for any duration that divides a day — which ValidBucketDuration is what
// guarantees. The UTC conversion is not cosmetic: Truncate on a wall clock in a
// zone with a non-hour offset would floor onto that zone's boundaries.
func BucketStart(t time.Time, d time.Duration) time.Time {
	return t.UTC().Truncate(d)
}

// Bucket returns the half-open span [start, start+d) containing t.
func Bucket(t time.Time, d time.Duration) (start, end time.Time) {
	start = BucketStart(t, d)
	return start, start.Add(d)
}

// AdvisoryLockKey derives the two 32-bit halves Postgres wants for
// pg_advisory_xact_lock(int, int).
//
// Two keys, not one hashed pair, so the satellite half is legible in
// pg_locks during an incident: classid is the satellite's hash and objid the
// bucket's, and a stuck round can be attributed without decoding anything.
//
// COLLISIONS ARE SAFE AND ARE NOT PREVENTED. Two distinct round keys that hash
// alike would serialise against each other unnecessarily — slower, never
// wrong — because the lock is a mutual-exclusion device and not an identity.
// The alternative, a lock table keyed on the real values, would need a row to
// exist for a bucket that has never been planned, and inventing a placeholder
// row to lock is a worse design than locking the concept directly.
func AdvisoryLockKey(k RoundKey) (satellite, bucket int32) {
	h := fnv.New32a()
	// Hash.Write never returns an error; the interface says so explicitly.
	_, _ = h.Write([]byte(k.SatelliteID))
	//nolint:gosec // the truncation is the point: a 32-bit lock half
	satellite = int32(h.Sum32())

	// Unix seconds rather than the hash of a formatted string. Bucket starts
	// are already sparse and evenly spaced, so they make a better key than
	// their own digest — and a debugging human can read the number back into a
	// timestamp, which a hash forbids.
	//nolint:gosec // same
	bucket = int32(k.BucketStart.UTC().Unix() / 60)
	return satellite, bucket
}
