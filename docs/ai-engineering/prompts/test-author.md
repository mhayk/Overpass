# test-author

Used for: designing verification, deliberately in a different session from the
implementation.

```
Design the tests for this unit of work. You did not write the implementation
and you should not assume it is correct.

Before writing any test, answer: WHAT IS THE ORACLE?
- If the answer is "the current output of the code", stop. That is a
  snapshot, not a test. It asserts the code still does what it did
  yesterday, including if yesterday was wrong.
- For physics: find published ground truth. Known passes, reference values,
  an independent implementation.
- For algorithms: state invariants that must hold for ANY input, and
  generate inputs. Examples only cover the cases someone thought of.
- For contracts: round-trip real payloads, and assert the negative cases
  are actually rejected.

Then tell me what your tests do NOT cover, and why.
```

## What made this work

**"What is the oracle?"** asked before anything else. It is the question that
separates a test from a snapshot, and for orbital mechanics it is the whole
ballgame: a snapshot of a wrong SGP4 implementation passes forever and enshrines
the error.

**"Tell me what your tests do NOT cover."** Produced the classification tables in
both round-trip suites — the explicit statement that neither binding enforces
`prefixItems`, so a swapped lat/lon parses cleanly in both languages while
failing the schema. That gap is now asserted rather than discovered later by
someone trusting their types.
