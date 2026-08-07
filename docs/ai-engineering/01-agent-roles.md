# Agent roles

Five roles, each with a different job and — critically — a different *stance*
toward the code. The reason for separating them is not tidiness. It is that a
session which just wrote a function is the worst possible reviewer of that
function: it has already committed to an interpretation, and it will defend it.

Prompts are in [`prompts/`](prompts/).

| Role | Job | Stance |
| --- | --- | --- |
| **spec-writer** | Contracts, ADRs, issue decomposition | Decide and justify. Every choice names its rejected alternatives. |
| **implementer** | Build one issue against a frozen contract | Boring and explicit. No abstraction without a second caller. |
| **test-author** | Design the verification for a unit of work | Assume the implementation is wrong. Find the oracle first. |
| **adversarial-reviewer** | Attack finished work | Assume it is broken and prove it. No credit for it looking fine. |
| **docs** | README, write-ups, PR narrative | Explain the reasoning, not the mechanics. |

## Why the split matters

**spec-writer runs to completion before implementer starts.** This is the whole
parallelism argument from [00-methodology.md](00-methodology.md): with contracts
frozen, three implementer sessions cannot collide. Without it they invent three
compatible-looking, incompatible interpretations.

**test-author is not the implementer.** If the same session writes the geometry
and its tests, the tests encode the same misunderstanding — and a snapshot of a
wrong answer passes forever. This is the single most important separation in the
list, and it is why `risk/high` issues specify an *independent oracle* rather
than expected values derived from running the code.

**adversarial-reviewer is prompted to break things, not to approve them.** A
reviewer asked "does this look right?" answers yes, because it does look right;
that is precisely the failure mode. Asked "find the input that makes this
produce a wrong answer without erroring", it produces something useful. Three of
the four M0 defects in [02-verification.md](02-verification.md) came from this
stance — deliberately breaking a schema to see whether the drift gate would
notice, rather than reading the drift gate and concluding it was fine.

## Handoff

State travels in the repository, never in session memory:

- **`CLAUDE.md`** — the standing agreement, loaded every session
- **The issue** — context, acceptance criteria, and the mandatory *Engineering
  decisions* section, which is how a later session inherits reasoning instead of
  re-deriving it and landing somewhere subtly different
- **ADRs** — decisions with their rejected alternatives
- **The contracts** — the interface, enforced by CI rather than by agreement

A session that needs information not in one of those four places is a session
about to guess. That is the signal to write it down first.
