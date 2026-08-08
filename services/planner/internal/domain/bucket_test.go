package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/domain"
)

// "Aligned to UTC" is only true if the duration divides a day. A 7-hour bucket
// tiles forward from the epoch and drifts across midnight, so the same clock
// time means different spans on different days — and every individual round
// still works, which is exactly why it needs asserting.
func TestValidBucketDuration(t *testing.T) {
	valid := []time.Duration{
		time.Minute, 15 * time.Minute, 30 * time.Minute,
		time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour,
		8 * time.Hour, 12 * time.Hour, 24 * time.Hour,
	}
	for _, d := range valid {
		if err := domain.ValidBucketDuration(d); err != nil {
			t.Errorf("%s divides a day and was refused: %v", d, err)
		}
	}

	invalid := []time.Duration{
		0, -time.Hour,
		5 * time.Hour,  // 24/5 is not whole
		7 * time.Hour,  // the drifting case
		9 * time.Hour,  //
		25 * time.Hour, // longer than a day
		7 * time.Minute,
	}
	for _, d := range invalid {
		err := domain.ValidBucketDuration(d)
		if err == nil {
			t.Errorf("%s does not divide a day and was accepted", d)
			continue
		}
		if !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: error does not wrap ErrInvalid: %v", d, err)
		}
	}
}

func TestBucketStartFloorsToTheBoundary(t *testing.T) {
	const d = 3 * time.Hour

	tests := []struct {
		name string
		at   time.Time
		want time.Time
	}{
		{"exactly on a boundary stays put",
			time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),
			time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)},
		{"a second past the boundary floors back",
			time.Date(2026, 8, 8, 12, 0, 1, 0, time.UTC),
			time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)},
		{"a second before floors to the previous bucket",
			time.Date(2026, 8, 8, 11, 59, 59, 0, time.UTC),
			time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)},
		{"midnight is a boundary",
			time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)},
		{"the last instant of the day",
			time.Date(2026, 8, 8, 23, 59, 59, 999999999, time.UTC),
			time.Date(2026, 8, 8, 21, 0, 0, 0, time.UTC)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := domain.BucketStart(tt.at, d); !got.Equal(tt.want) {
				t.Errorf("BucketStart(%s) = %s, want %s", tt.at, got, tt.want)
			}
		})
	}
}

// The UTC conversion is not cosmetic. Truncate on a wall clock in a zone with a
// non-hour offset floors onto THAT zone's boundaries, which would put the same
// instant in different buckets depending on the server's TZ — and the bucket is
// half the advisory-lock key.
func TestBucketStartIgnoresTheInputZone(t *testing.T) {
	const d = 3 * time.Hour
	kathmandu := time.FixedZone("NPT", 5*3600+45*60) // +05:45, deliberately not a whole hour

	instant := time.Date(2026, 8, 8, 13, 30, 0, 0, time.UTC)
	local := instant.In(kathmandu)

	fromUTC := domain.BucketStart(instant, d)
	fromLocal := domain.BucketStart(local, d)

	if !fromUTC.Equal(fromLocal) {
		t.Errorf("the same instant bucketed to %s from UTC and %s from +05:45; the lock key depends on the server's timezone",
			fromUTC, fromLocal)
	}
	if fromUTC.Location() != time.UTC {
		t.Errorf("bucket start is in %s, not UTC", fromUTC.Location())
	}
}

func TestBucketIsHalfOpen(t *testing.T) {
	const d = 3 * time.Hour
	start, end := domain.Bucket(time.Date(2026, 8, 8, 13, 30, 0, 0, time.UTC), d)

	if want := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Errorf("start = %s, want %s", start, want)
	}
	if end.Sub(start) != d {
		t.Errorf("span = %s, want %s", end.Sub(start), d)
	}
	// The end must be the NEXT bucket's start, or an instant falls into two
	// buckets or none.
	if next := domain.BucketStart(end, d); !next.Equal(end) {
		t.Errorf("the bucket end %s is not itself a boundary (%s)", end, next)
	}
}

func TestAdvisoryLockKeySeparatesSatellitesAndBuckets(t *testing.T) {
	bucket := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	other := bucket.Add(3 * time.Hour)

	aSat, aBucket := domain.AdvisoryLockKey(domain.RoundKey{SatelliteID: "CAPELLA-14", BucketStart: bucket})
	bSat, bBucket := domain.AdvisoryLockKey(domain.RoundKey{SatelliteID: "CAPELLA-15", BucketStart: bucket})
	cSat, cBucket := domain.AdvisoryLockKey(domain.RoundKey{SatelliteID: "CAPELLA-14", BucketStart: other})

	// Different satellites must not serialise against each other — that is the
	// property that stops per-satellite locking becoming a global ceiling.
	if aSat == bSat && aBucket == bBucket {
		t.Error("two satellites share a lock key; the whole constellation would serialise")
	}
	// Different buckets on one satellite are independent rounds.
	if aSat == cSat && aBucket == cBucket {
		t.Error("two buckets on one satellite share a lock key")
	}
	// And the key must be stable, or a round could not re-acquire its own lock.
	reSat, reBucket := domain.AdvisoryLockKey(domain.RoundKey{SatelliteID: "CAPELLA-14", BucketStart: bucket})
	if reSat != aSat || reBucket != aBucket {
		t.Error("the lock key is not stable across calls")
	}
}

// The bucket half is readable back into a timestamp on purpose — a hash would
// make an incident in pg_locks undecodable.
func TestTheBucketHalfOfTheLockKeyIsReadable(t *testing.T) {
	bucket := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	_, bucketKey := domain.AdvisoryLockKey(domain.RoundKey{SatelliteID: "CAPELLA-14", BucketStart: bucket})

	if got := time.Unix(int64(bucketKey)*60, 0).UTC(); !got.Equal(bucket) {
		t.Errorf("the lock key decodes to %s, want %s", got, bucket)
	}
}

func TestRoundKeyStringIsLegible(t *testing.T) {
	k := domain.RoundKey{
		SatelliteID: "CAPELLA-14",
		BucketStart: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),
	}
	if want := "CAPELLA-14@2026-08-08T12:00:00Z"; k.String() != want {
		t.Errorf("String() = %q, want %q", k, want)
	}
}
