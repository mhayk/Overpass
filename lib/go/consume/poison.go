package consume

import "fmt"

// Poison detection: deciding to STOP retrying before the broker's max_deliver
// budget is silently exhausted.
//
// The distinction this file draws is between two failures that look identical
// at the call site and must not be treated identically:
//
//	retryable  — the database was down, the network blinked. Redelivery is the
//	             cure, and Nak is the right answer.
//	poison     — the payload can never succeed: malformed, contract-invalid,
//	             semantically impossible. Redelivery re-runs the same failure,
//	             burns a delivery slot each time, and after max_deliver the
//	             broker drops the message WITHOUT ANYONE DECIDING TO — the
//	             worst outcome, because it looks like handling and is loss.
//
// Term is the deliberate version of that drop: it stops delivery NOW, on
// purpose, with a log line saying why, and leaves the max_deliver budget as
// headroom for genuinely transient failures rather than as a fuse that poison
// quietly burns.

// Decision says what to do with a failed delivery.
type Decision int

const (
	// Retry: Nak the message and let the broker redeliver.
	Retry Decision = iota
	// Terminate: Term the message — poison, or the last delivery of a failure
	// that retrying has not fixed. Deliberate, logged, final.
	Terminate
)

// OnFailure decides Retry or Terminate for a failed delivery.
//
// permanent says the handler classified the failure as impossible to fix by
// retrying — a decode error, a contract violation. Those terminate on the
// FIRST failure: rerunning a deterministic failure max_deliver times adds
// nothing but log noise and delivery latency for the messages behind it.
//
// Everything else retries until the LAST delivery. Terminating on the final
// attempt rather than letting max_deliver lapse is the difference between a
// decision with a log line and a silent drop: the broker's counter reaching
// zero pages nobody.
func OnFailure(permanent bool, delivered uint64, maxDeliver int) Decision {
	if permanent {
		return Terminate
	}
	if maxDeliver > 0 && delivered >= uint64(maxDeliver) {
		// This WAS the last attempt: the broker will not redeliver after it.
		// Term instead of a lapsed budget, so the drop is chosen and logged.
		return Terminate
	}
	return Retry
}

// String makes decisions legible in logs.
func (d Decision) String() string {
	switch d {
	case Retry:
		return "retry"
	case Terminate:
		return "terminate"
	default:
		return fmt.Sprintf("decision(%d)", int(d))
	}
}
