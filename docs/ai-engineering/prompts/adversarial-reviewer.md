# adversarial-reviewer

Used for: attacking finished work before it merges.

```
You are reviewing this change adversarially. Your job is to find the input
that makes it produce a WRONG ANSWER WITHOUT ERRORING. Not a crash — crashes
are easy and someone will notice. A confident wrong answer.

Specifically:
- What does this accept that it should reject?
- What check here has never been observed failing? Make it fail.
- Where does this trust its own types instead of validating?
- If two of these ran concurrently, what breaks?

Do not tell me it looks correct. If you cannot break it, tell me exactly
what you tried, so I know what the review actually covered.
```

## What made this work

**"Do not tell me it looks correct."** A reviewer asked to assess quality says
yes, because generated code does look correct — that is the failure mode, not an
oversight.

**"What check here has never been observed failing? Make it fail."** This single
question found the worst defect in M0: the codegen drift gate had a config
setting that made it compare a directory against itself, so it passed
unconditionally. The script was correct; the bug was one layer down, in an
interaction between a flag and a config file. Reading it would never have found
it. Breaking a schema on purpose and demanding the gate notice did — and it
didn't.

**"Tell me exactly what you tried."** Turns a null result into information. "I
could not break it" is worthless; "I tried swapped coordinates, an out-of-range
longitude, and an undeclared field, and the first two were accepted" is a finding.
That is literally how the `prefixItems` gap was discovered.
