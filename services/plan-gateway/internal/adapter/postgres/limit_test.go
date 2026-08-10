package postgres

import "testing"

// Asking for more must never quietly yield LESS.
//
// limitOr used to return 50 for anything above 200. The geo handlers cap at
// 250 and pass limit+1 = 251 so truncation is detectable, so every geo query
// silently came back with 50 rows — and the handler then computed
// `truncated = 50 > 250` as false. The API said "here is everything" while
// returning 50 of 293.
//
// This is in the internal package rather than postgres_test on purpose:
// limitOr is unexported, the defect was entirely in its arithmetic, and a test
// that needed a database to catch it would not have been written.
func TestLimitOrNeverReturnsLessThanAsked(t *testing.T) {
	for _, asked := range []int{1, 49, 50, 51, 200, 201, 251, 999, maxRows} {
		if got := limitOr(asked); got < asked {
			t.Errorf("limitOr(%d) = %d — asking for more must not yield less", asked, got)
		}
	}
}

// The geo handlers' actual value. Named explicitly because it is the one that
// was broken in production code and would be the first to regress.
func TestLimitOrHandlesTheGeoHandlersLimitPlusOne(t *testing.T) {
	const geoLimitPlusOne = 251
	if got := limitOr(geoLimitPlusOne); got != geoLimitPlusOne {
		t.Errorf("limitOr(%d) = %d, want %d — this is the value every geo query passes",
			geoLimitPlusOne, got, geoLimitPlusOne)
	}
}

func TestLimitOrDefaultsWhenUnset(t *testing.T) {
	for _, unset := range []int{0, -1} {
		if got := limitOr(unset); got != defaultRows {
			t.Errorf("limitOr(%d) = %d, want %d", unset, got, defaultRows)
		}
	}
}

// The ceiling still exists: this is a safety net against a caller asking for
// the whole table, not a suggestion.
func TestLimitOrClampsAtTheCeiling(t *testing.T) {
	if got := limitOr(maxRows + 1); got != maxRows {
		t.Errorf("limitOr(%d) = %d, want %d", maxRows+1, got, maxRows)
	}
}
