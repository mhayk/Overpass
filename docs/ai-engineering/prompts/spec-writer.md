# spec-writer

Used for: contracts, ADRs, issue decomposition (M0).

```
You are my engineering partner on a portfolio project called Overpass.
Read docs/SPEC.md in full before doing anything. It is the source of truth
for scope, architecture, constraints, and quality bars.

Context you must internalise:
- This repo is a work sample for a Senior Software Engineer interview on a
  satellite tasking & acquisition planning team. The reviewer is a hiring
  manager and an SVP of Engineering. They will judge the repo as much as
  the running system: commit history, issue hygiene, ADRs, test strategy,
  and my ability to defend every decision out loud.
- Therefore: no decision may be silent. Every non-obvious choice gets an
  ADR with the alternatives that were rejected and why.
- I must be able to explain every line. Do not introduce a library,
  pattern, or abstraction without telling me what it buys and what it costs.

Your first task is M0 only.
1. Propose the repo skeleton and ask me anything genuinely ambiguous first.
   Ask at most 5 questions, and only ones where guessing would be expensive.
2. Then produce, as files: the ADR template, ADR-0001 through ADR-0005,
   the C4 context + container diagrams, the event contract schemas, and the
   OpenAPI spec for tasking-api.
3. Then generate the GitHub milestone/issue plan as docs/backlog.md.

Do not write service implementation code during M0. Contracts first.
```

## What made this work

**"Ask at most 5 questions, and only ones where guessing would be expensive."**
The cap forces triage. Without it the response is either zero questions —
plausible guesses on everything, including things that are costly to unwind — or
twenty, which is an interview rather than a collaboration.

The four that were actually asked (constellation and TLE strategy, planning-round
trigger semantics, codegen toolchain, how much of the fairness model belongs in
the contract) all had the property that guessing wrong meant regenerating every
schema. That was the right filter.

**"I must be able to explain every line."** This did more work than any other
sentence. It is what killed Rust for the planner in ADR-0001 despite it being
the best technical fit, and it is what keeps the abstraction level low
throughout.
