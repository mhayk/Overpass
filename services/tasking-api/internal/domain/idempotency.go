package domain

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Fingerprint is the digest of a canonicalised request body.
//
// A DIGEST OF THE CANONICAL FORM, not of the raw bytes. Two submissions that
// differ only in key order or whitespace are the same submission, and a client
// that reserialises before retrying — which many HTTP libraries do — would
// otherwise get a 409 for a retry that is genuinely identical.
//
// The point of storing it at all: key reuse with different content is a client
// bug. Treating it as a replay silently discards a request the customer
// believes they submitted, and they find out when the image never arrives.
type Fingerprint [32]byte

// String renders the digest for storage and logs.
func (f Fingerprint) String() string { return fmt.Sprintf("%x", f[:]) }

// FingerprintBody canonicalises a JSON body and digests it.
//
// Returns an error rather than digesting the raw bytes on failure. A
// fingerprint over bytes we could not parse would compare unequal to itself
// after any reserialisation, turning every retry into a 409.
func FingerprintBody(raw []byte) (Fingerprint, error) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	// Numbers as strings, so 1200 and 1200.0 and 1.2e3 do not become the same
	// float and then the same digest. They are different bytes from the client
	// and treating them as identical is a guess we do not need to make.
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return Fingerprint{}, fmt.Errorf("body is not JSON: %w", err)
	}

	var b strings.Builder
	writeCanonical(&b, value)
	return sha256.Sum256([]byte(b.String())), nil
}

// writeCanonical writes a deterministic rendering of a decoded JSON value.
//
// Object keys sorted, no insignificant whitespace, arrays left in order —
// array order is meaningful in JSON and sorting one would make two genuinely
// different requests fingerprint the same.
func writeCanonical(b *strings.Builder, v any) {
	switch value := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for k := range value {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Quote(k))
			b.WriteByte(':')
			writeCanonical(b, value[k])
		}
		b.WriteByte('}')

	case []any:
		b.WriteByte('[')
		for i, item := range value {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonical(b, item)
		}
		b.WriteByte(']')

	case string:
		b.WriteString(strconv.Quote(value))

	case json.Number:
		b.WriteString(value.String())

	case bool:
		b.WriteString(strconv.FormatBool(value))

	case nil:
		b.WriteString("null")

	default:
		// Unreachable with UseNumber, but silence beats a panic in an ingress
		// path: an unexpected type digests as its Go rendering, which is stable.
		fmt.Fprintf(b, "%v", value)
	}
}

// IdempotencyKeyValid reports whether a key matches the contract's pattern.
//
// Validated here rather than trusted, because the key becomes half of a primary
// key and an unbounded one is an unbounded row.
func IdempotencyKeyValid(key string) bool {
	if len(key) < 8 || len(key) > 128 {
		return false
	}
	for _, r := range key {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '~' || r == '-':
		default:
			return false
		}
	}
	return true
}
