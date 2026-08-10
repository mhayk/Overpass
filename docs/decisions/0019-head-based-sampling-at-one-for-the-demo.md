# 0019 — Head-based sampling, at 1.0 for the demo and configurable everywhere

- **Status:** accepted
- **Date:** 2026-08-10
- **Deciders:** Mhayk Whandson

## Context and problem statement

M3-05 asks for sampling to be "configured and justified". Every service already
read `TRACE_SAMPLE_RATIO` and defaulted it to 1.0 — so it was configured, and
nowhere justified, which is the half that matters. A rate nobody argued for is a
number someone will change without knowing what it protects.

The question: **what sampling strategy does a system whose traces span two
languages and three async boundaries need, and what rate should the committed
stack run at?**

## Decision drivers

- **A partial trace is worse than no trace.** This system's whole tracing claim
  is that ONE trace covers HTTP ingress → outbox → Python consumer → publish →
  Go planner → commit → projector. A strategy that can sample a middle hop
  independently destroys exactly the property being demonstrated.
- **The demo is the product.** A reviewer runs `make demo` and opens Tempo. A
  trace that is missing because it lost a coin flip makes the system look
  broken, and no explanation recovers that first impression.
- **Trace volume nobody stores gets the stack turned off.** The issue's own
  engineering note: always-on at 1000 rps produces volume nobody will pay for,
  and an unaffordable observability stack is switched off, which is worse than
  sampling.
- Local Docker Compose retains traces for hours, not weeks. The storage argument
  that dominates in production barely exists here.

## Considered options

1. **Head-based, ParentBased, ratio configurable, default 1.0.**
2. **Head-based at a fixed fraction** — e.g. 0.1 everywhere.
3. **Tail-based sampling in the collector** — decide after the trace completes,
   keeping errors and slow traces.

## Decision outcome

Chosen: **Option 1**, with `ParentBased(TraceIDRatioBased(ratio))` in all four
services and `TRACE_SAMPLE_RATIO` defaulting to 1.0.

**ParentBased is the load-bearing half, not the ratio.** It means a sampling
decision made at ingress is HONOURED downstream rather than re-rolled per
service. Re-rolling is how a trace ends up with holes in the middle: each hop
independently decides, and at ratio r a five-hop trace survives intact with
probability r⁵. At r=0.1 that is one trace in a hundred thousand — the
end-to-end trace this milestone exists to demonstrate would essentially never
appear, while each service's own dashboard looked fine.

The ratio is 1.0 in the committed stack because this is a demo: a reviewer runs
`make demo` once and opens Tempo once, and a trace missing on a coin flip reads
as a broken system. The variable exists so that a deployment which is not a demo
changes one environment value rather than a line of code in four services.

*Rejected:* Option 2, a fixed fraction. It buys nothing here — the local stack's
trace volume is a few hundred spans per demo — and costs the demo its
reliability. "Configured and justified" does not mean "smaller than one".

*Rejected:* Option 3, tail-based sampling. It is the technically better answer
for production and the wrong one to adopt now. It requires the collector to
buffer whole traces, adds a `tail_sampling` processor with policies that need
tuning against real traffic this project does not have, and its benefit —
keeping the interesting traces rather than a random subset — is invisible at a
volume where everything is kept anyway. Adopting it here would be tuning a
mechanism against imagined load, which is the prediction-dressed-as-decision
ADR-0007 already refuses.

The upgrade path is real and cheap: tail sampling is a collector-side change and
needs no service to be rebuilt. That is a consequence of the ADR-0018 decision
to push everything through the collector, and it is worth stating that the two
decisions compose.

## Consequences

- One environment variable, `TRACE_SAMPLE_RATIO`, understood identically by
  three Go services and one Python service, all validated to 0..1 and refusing
  anything outside it rather than clamping. A value of 2 means whoever set it
  believes something untrue about the knob, and silently treating it as 1.0
  leaves that belief in place.
- Metrics are NOT sampled, and that is not an oversight. A sampled counter is a
  wrong number rather than an approximate one, and the domain metrics in
  ADR-0018 are counts a customer's bill could depend on.
- At 1.0 the exporters carry every span, so a collector outage costs a visible
  drop rather than a statistically invisible one. Acceptable, and preferable for
  a stack whose failures are meant to be legible.
