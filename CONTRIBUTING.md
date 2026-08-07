# Contributing

## Workflow

One issue, one branch, one PR, squash-merge.

```bash
gh issue develop 42 --checkout        # branch named from the issue
# ... work ...
git commit -m "feat(planner): ..."    # Conventional Commits
gh pr create                          # body must contain "Closes #42"
```

Squash-merge with linear history, so the commit log reads as one commit per
issue. That is deliberate: the history is read as closely as the code, and a
graph full of merge bubbles and "wip" commits is not a narrative.

## Branch naming

```
<type>/<issue-number>-<short-slug>

feat/34-planner-round-trigger
fix/61-slew-time-asymmetry
docs/12-adr-0010-test-strategy
chore/8-compose-skeleton
```

## Commits

[Conventional Commits](https://www.conventionalcommits.org/).
Types: `feat`, `fix`, `docs`, `chore`, `ci`, `test`, `perf`, `refactor`.
Scope is the service or area: `planner`, `contracts`, `infra`, `web`.

**The body matters more than the subject.** Explain why, what was rejected, and
what it costs. A commit that only says what changed is a diff with extra steps —
the diff already says what changed.

## Before you open a PR

```bash
make contracts        # validate, regenerate, round-trip
make lint
make test
```

If you touched a schema, `gen/` must be regenerated and committed. CI fails
otherwise, by design.

## The rule that is easy to skip

**If your change adds or modifies a check — a gate, a constraint, a validator —
you must have watched it fail on the thing it is supposed to catch.**

Not reasoned about. Observed. Break the input on purpose, confirm the check
notices, then restore it and confirm it passes.

This is not pedantry. The codegen drift gate in #7 had a config setting that made
it compare a directory against itself; it passed unconditionally and would have
kept passing forever. The script was correct — the bug was one layer below it,
invisible to reading. A gate never seen failing is not known to work, and a
broken gate is worse than no gate because it is trusted.

## Contracts

`contracts/` is the source of truth. Read `contracts/README.md` before changing
anything in it — several non-obvious conventions there exist because of specific
generator behaviour, and reverting one silently breaks a language binding.

**Never edit a published schema in place.** Add a `.v2` file and a `.v2` subject.
Adding an optional field is a minor version bump; anything else is a new major
line.

## One track per change

CI enforces the [ADR-0013](docs/decisions/0013-parallel-agent-execution-in-worktrees.md)
path-ownership rule, so that several people or agents can work in parallel
without silently moving something the others are building against. Two rules,
both decided from your diff alone:

1. **At most one track directory per change.** The tracks are
   `services/tasking-api/`, `services/feasibility/`, `services/planner/`,
   `services/plan-gateway/`, and `web/`.
2. **A shared-fate path lands on its own.** Those are `contracts/`, `gen/`,
   `db/migrations/`, `CLAUDE.md`, and `docs/decisions/`. Contract changes
   serialise — that is what M0 bought. `contracts/` and `gen/` together are fine,
   because `make contracts` moves both as one unit.

Everything else — `Makefile`, `scripts/`, `deploy/`, `docs/` outside
`docs/decisions/`, `.github/` — is common, and a track may touch it freely.

Check before you push:

    git diff --name-only origin/main... | ./scripts/check-path-ownership.sh

**If the crossing is deliberate**, label the pull request `crosses-tracks` and
the gate stands down. Integration work that genuinely spans services — the
end-to-end tests, the demo seed, tracing across every hop — is supposed to use
this. The hatch exists because the point of the rule is that crossings are
*visible*, not that they are impossible; a labelled crossing gets read in review,
a silent one does not.

## Decisions

Every non-obvious choice gets recorded, at the scale that fits:

| Scale | Where |
| --- | --- |
| Shapes the architecture | An ADR in `docs/decisions/` |
| Shapes one unit of work | The issue's **Engineering decisions** section |
| Shapes one change | The PR body |

Each requires the alternatives you rejected and why. A decision with no rejected
alternatives was not a decision.

## Review

Reviews here are adversarial by intent. The useful question is not "does this
look right?" — generated and hand-written code both look right, which is the
whole problem. It is:

- What does this accept that it should reject?
- Which check here has never been observed failing?
- Where does this trust its own types instead of validating?
- If two of these ran concurrently, what breaks?
