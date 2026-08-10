// The one place a tasking request body is built.
//
// Shared by every scenario so that a change to the contract breaks one file
// rather than four, and so the scenarios differ only in their LOAD SHAPE —
// which is the variable each of them is actually about.

const CUSTOMERS = [
  'acme-imaging',
  'nl-coastguard',
  'eu-civil-protection',
  'port-authority-nl',
];

const TIERS = ['BEST_EFFORT', 'COMMERCIAL', 'CIVIL_PROTECTION', 'GOVERNMENT'];

// Rotterdam, the demo's target. Jittered per request by up to ~0.5 degrees so
// the load is not one identical row repeated: identical targets would let
// Postgres serve every access-window query from the same cached plan and make
// the ingress path look faster than it is.
const CENTRE = [4.4, 51.9];

export function body(index) {
  const jitter = () => (Math.random() - 0.5);
  const start = new Date(Date.now() + 3600_000);
  const end = new Date(Date.now() + 25 * 3600_000);

  return JSON.stringify({
    customer_id: CUSTOMERS[index % CUSTOMERS.length],
    target_name: `loadtest ${index}`,
    target: {
      type: 'Point',
      coordinates: [CENTRE[0] + jitter(), CENTRE[1] + jitter()],
    },
    window: { start: start.toISOString(), end: end.toISOString() },
    priority_tier: TIERS[index % TIERS.length],
    bid_credits: 100 + (index % 900),
    requested_modes: ['SCAN'],
  });
}

// A UNIQUE key per request, deliberately.
//
// Reusing one would exercise the idempotency REPLAY path — a cheap ledger hit
// that returns the stored response without touching the write path — and the
// scenarios below are about the write path. Measuring replays and calling it
// ingress throughput is the most flattering possible mistake, so the key
// carries the VU id and the iteration.
export function idempotencyKey(vu, iteration, prefix) {
  return `${prefix}-${vu}-${iteration}-${Date.now()}`;
}

export function headers(key) {
  return {
    'Content-Type': 'application/json',
    'Idempotency-Key': key,
  };
}
