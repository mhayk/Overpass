Closes #

## What this does

<!-- One paragraph. What changed and why it was worth changing. -->

## Engineering decisions

<!--
Mandatory, even if the answer is "none — this follows the pattern established
in #N". The commit history is read as closely as the code, and a PR that
records its reasoning is the difference between a narrative and a changelog.

For each non-obvious choice: what was chosen, what was rejected, what it costs.
If it needs an ADR, say which number and open it.
-->

## Verification

<!--
Not "tests pass" — what did you actually run, and what did it prove?

If this change adds or modifies a CHECK (a gate, a constraint, a validator),
you must have seen it FAIL on the thing it is supposed to catch. Say so, and
say how. A gate never observed failing is not known to work; that lesson cost
us the codegen drift gate in #7.
-->

```
$ 
```

## Risk

<!--
Which of these does this change touch? Each has a silent failure mode, so each
requires more than an example test — see ADR-0010.

- [ ] Orbital geometry — golden-reference test against an independent oracle
- [ ] Allocation or scheduling invariants — property-based test
- [ ] Concurrency, idempotency, or delivery semantics — integration test against
      real Postgres and NATS
- [ ] A published contract — versioned, not edited in place
- [ ] None of the above
-->

## Scope cuts

<!-- Anything deliberately left out, so its absence is a decision rather than an
oversight. Write "none" if none. -->
