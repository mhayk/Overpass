package domain_test

import (
	"strings"
	"testing"

	"github.com/mhayk/overpass/services/tasking-api/internal/domain"
)

func mustFingerprint(t *testing.T, body string) domain.Fingerprint {
	t.Helper()
	f, err := domain.FingerprintBody([]byte(body))
	if err != nil {
		t.Fatalf("fingerprinting %q: %v", body, err)
	}
	return f
}

func TestKeyOrderDoesNotChangeTheFingerprint(t *testing.T) {
	// The reason the digest is over the canonical form. Many HTTP clients
	// reserialise before retrying, and a digest over raw bytes would turn a
	// genuinely identical retry into a 409.
	a := mustFingerprint(t, `{"b":2,"a":1}`)
	b := mustFingerprint(t, `{"a":1,"b":2}`)
	if a != b {
		t.Fatal("reordering keys changed the fingerprint")
	}
}

func TestWhitespaceDoesNotChangeTheFingerprint(t *testing.T) {
	a := mustFingerprint(t, `{"a":1,"b":[1,2]}`)
	b := mustFingerprint(t, "{\n  \"a\" : 1,\n  \"b\" : [ 1, 2 ]\n}")
	if a != b {
		t.Fatal("whitespace changed the fingerprint")
	}
}

func TestNestedKeyOrderAlsoCanonicalises(t *testing.T) {
	a := mustFingerprint(t, `{"outer":{"z":1,"a":2}}`)
	b := mustFingerprint(t, `{"outer":{"a":2,"z":1}}`)
	if a != b {
		t.Fatal("nested keys were not sorted")
	}
}

func TestArrayOrderIsSignificant(t *testing.T) {
	// Sorting arrays would make two genuinely different requests fingerprint
	// the same — [SPOTLIGHT, STRIPMAP] is not the same preference as the
	// reverse.
	if mustFingerprint(t, `{"m":[1,2]}`) == mustFingerprint(t, `{"m":[2,1]}`) {
		t.Fatal("array order was ignored")
	}
}

func TestDifferentValuesDiffer(t *testing.T) {
	if mustFingerprint(t, `{"bid":1200}`) == mustFingerprint(t, `{"bid":9999}`) {
		t.Fatal("two different bodies fingerprinted the same")
	}
}

func TestNumbersAreComparedAsWritten(t *testing.T) {
	// 1200 and 1.2e3 are the same number and different bytes from the client.
	// Collapsing them to a float would be a guess we do not need to make, and
	// the safe direction is to treat them as different submissions.
	if mustFingerprint(t, `{"n":1200}`) == mustFingerprint(t, `{"n":1.2e3}`) {
		t.Fatal("distinct numeric literals were collapsed")
	}
}

func TestANonJSONBodyIsAnError(t *testing.T) {
	// Digesting bytes we could not parse would compare unequal to itself after
	// any reserialisation, turning every retry into a 409.
	if _, err := domain.FingerprintBody([]byte(`not json`)); err == nil {
		t.Fatal("a non-JSON body was fingerprinted")
	}
}

func TestTheFingerprintRendersAsHex(t *testing.T) {
	got := mustFingerprint(t, `{"a":1}`).String()
	if len(got) != 64 {
		t.Fatalf("digest rendered as %d characters, want 64", len(got))
	}
	if strings.ContainsAny(got, "ghijklmnopqrstuvwxyz") {
		t.Fatalf("digest is not hex: %s", got)
	}
}

func TestIdempotencyKeyValidation(t *testing.T) {
	valid := []string{"abcdefgh", "a.b_c~d-e", strings.Repeat("k", 128)}
	for _, key := range valid {
		if !domain.IdempotencyKeyValid(key) {
			t.Errorf("%q should be valid", key)
		}
	}

	invalid := []string{
		"",                       // absent
		"short",                  // under 8
		strings.Repeat("k", 129), // over 128
		"has space",              // space
		"has/slash",              // not in the permitted set
		"haséaccent",             // not ASCII
	}
	for _, key := range invalid {
		if domain.IdempotencyKeyValid(key) {
			t.Errorf("%q should be rejected", key)
		}
	}
}
