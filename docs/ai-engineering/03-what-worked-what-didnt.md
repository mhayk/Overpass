# What worked, what didn't

Written continuously, not retrofitted at the end. Retrofitted honesty reads as
retrofitted.

Covers M0 only so far. Updated as later milestones land — including when the
answer is unflattering.

---

## What worked

### Contracts-first, by a wide margin

Spending a whole milestone on schemas before any service existed felt slow and
was not. The payoff arrived immediately and concretely: four defects
([02-verification.md](02-verification.md)) were caught **before a single service
consumed a contract**. Finding the timestamp decoding bug in M1, inside a running
consumer, would have looked like a mysterious message-handling failure rather
than an obvious type problem.

### Making the model produce *decisions*, not code

The instruction that did the most work was not about code at all:

> I must be able to explain every line. Do not introduce a library, pattern, or
> abstraction without telling me what it buys and what it costs.

It changed the output shape. Every ADR names its rejected alternatives; the
generated code stayed unabstracted; and Rust was rejected for the planner despite
being the best technical fit, on the explicit grounds that I could not defend
idiomatic Rust under questioning. That is the correct engineering answer and it
is not the answer an unconstrained model gives.

### Asking few questions, chosen for cost

Capping clarifying questions at five forced triage. The four that got asked all
shared one property: **guessing wrong meant regenerating every schema.** Round
trigger semantics, TLE strategy, codegen toolchain, and where the fairness model
lives. Everything else was decided with a stated assumption and flagged.

### Executable documentation

`make contracts-validate` is 32 checks, not a paragraph claiming the schemas are
fine. The cold-start requirement is a CI job that fails, not a sentence in a
README. Every claim that could be a gate is one, which means the documentation
cannot drift from reality without the build going red.

### Adversarial review as a separate stance

Asking "what check here has never been observed failing? Make it fail" found the
worst bug in M0 — the drift gate that could not fail. Reading the gate would
never have found it. The gate was correct; the bug was one layer below it.

---

## What didn't work, and what it cost

### Generated code that is fluent and wrong — three times

The recurring cost, and it is not small. Every one compiled, passed static
analysis, and looked idiomatic:

- `type OccurredAt time.Time` — a defined type, so no inherited JSON methods.
  Every timestamp in every event failed to decode.
- `format: uuid` plus a redundant `pattern` — pydantic `TypeError` at parse time.
  Every event carrying an id was unparseable in Python. Both keywords are
  individually correct.
- The drift gate comparing a directory against itself.

**Time lost: roughly two hours across the three**, most of it on the third,
because a broken checker misleads you into trusting everything downstream of it.

The lesson is narrow and useful: **for generated or AI-written code, reading is
not verification.** Not "read more carefully" — reading is the wrong tool. All
three defects were invisible to inspection and obvious to execution.

### Confident wrongness about tooling

More than one factual claim about a tool turned out to be wrong, and each cost a
round trip:

- `go-jsonschema` was assumed to resolve absolute `$ref` URIs. It resolves refs
  as *filesystem paths* and tried to HTTP-fetch `https://overpass.dev/...`,
  failing on the returned HTML. The schemas had to be rewritten to relative refs.
- It was assumed to handle `const`. It does not — it emits `interface{}`, which
  silently discarded the type safety a discriminator exists to provide. Every
  `const` is now a single-value `enum`.
- Version pins were asserted rather than checked. `v0.17.0` for `go-jsonschema`
  was invented; the actual output came from `v0.24.1`. **CI caught this, I did
  not** — the drift gate did its job on its first real run, reporting 1483 lines
  of difference.

**This is the cost the job description means by "where it quietly costs you
time."** The model states library behaviour with exactly the same confidence
whether it is recalling documentation or reconstructing something plausible, and
there is no signal in the text distinguishing the two. The only defence is to run
it. A `--version` check and one smoke generation would have saved every one of
these.

### Over-configuration on the first pass

The initial `.golangci.yml` enabled a large linter set including several that
only produce style opinions. It also targeted the wrong major version and had to
be rewritten for the v2 schema.

More generally: the first draft of nearly every config file was more elaborate
than necessary. Left alone, generated configuration reaches for completeness
rather than for the minimum that does the job — and every unnecessary setting is
one more thing to explain in an interview.

### The reserved-word trap

`window` is the obvious name for an acquisition's `tstzrange` column and is a
reserved word in Postgres. Caught only by running the SQL. Trivial, ten seconds
to fix — and it would have been ten seconds *plus a confusing migration failure*
if it had surfaced during M1 instead.

### Chained version incompatibilities

One dependency requiring Go 1.25 cascaded through three CI failures:
`kin-openapi` needs 1.25 → `gen/go` targets 1.25 → `golangci-lint` v1 (built with
Go 1.23) refuses to load it → upgrade to v2 → the v2 config schema differs →
`golangci-lint-action@v6` rejects v2 version strings → upgrade to v7.

Each fix was correct and each revealed the next. **Roughly 40 minutes**, and none
of it predictable from reading. Toolchain version graphs are exactly the kind of
thing a model reconstructs plausibly and wrongly, and the only way through is to
run it and read the actual error.

---

## The honest summary

AI assistance made M0 dramatically faster at producing **structure** — schemas,
ADRs, 70 issues with real acceptance criteria, three CI pipelines. That is
genuine and large.

It was consistently unreliable about **the behaviour of specific tools at
specific versions**, and it produced code that was wrong in ways invisible to
review. Both of those cost real time, and neither is fixed by better prompting.
They are fixed by executing everything and by building checks that are themselves
verified.

A candidate who says AI never misled them has not used it seriously. The useful
question is not whether it gets things wrong — it does, confidently — but whether
you have built something that catches it when it does. That is what
[ADR-0010](../decisions/0010-test-strategy-and-coverage.md) is for, and it is why
the test strategy is framed as a verification harness rather than as coverage.

## Still to find out

The two areas flagged highest-risk — **orbital geometry and concurrency safety** —
have not been written yet. Their verification is designed and their issues are
labelled, but the golden-reference tests and property-based invariants do not
exist. Those are where generated code is expected to fail hardest and least
visibly.

If the strategy works, M1 and M2 will produce specific, citable examples of it
catching something. If it does not, that goes here too.
