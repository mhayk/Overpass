# 0005 — Docker Compose as the deployment target, not Kubernetes

- **Status:** accepted
- **Date:** 2026-08-07
- **Deciders:** Mhayk Whandson

## Context and problem statement

Overpass is four services plus Postgres, NATS, an OpenTelemetry collector,
Prometheus, and Grafana. Something has to start all of that, wire it together,
seed it, and keep it running.

The definition of done in the spec is explicit and is the real constraint:

> `git clone && docker compose up` produces a working system with seeded data in
> under five minutes, on a clean machine.

That sentence is the requirement. Not "runs in production", not "scales to N
nodes" — *a reviewer on an unfamiliar laptop gets a working system in five
minutes.* Every orchestration decision has to serve it.

The question: **what is the minimum orchestration that satisfies that
requirement, and is anything more actually buying us something?**

## Decision drivers

1. **Time-to-first-working-system on a machine we do not control.** This is the
   dominant driver and it is not close.
2. **Zero prerequisite installation beyond Docker Desktop.** A reviewer who has to
   install `kind`, `kubectl`, `helm`, and `skaffold` before seeing anything will
   in practice not see anything.
3. **Debuggability during development.** Rebuilding one service and restarting it
   should take seconds.
4. **Honest representation of the deployment story.** Whatever we choose, the
   README must say what production would actually require. Pretending Compose is
   a production answer would be worse than choosing it openly.

## Considered options

1. **Docker Compose**
2. **Kubernetes via `kind`/`k3d`**, with manifests or a Helm chart in-repo
3. **Compose for local development + Kubernetes manifests as an unrun artifact**
4. **Nomad**, or another lighter-weight scheduler

## Decision outcome

Chosen: **Docker Compose, as the single deployment target.**

Kubernetes solves problems this project does not have. Its value is in
multi-node scheduling, rolling deploys with health-gated rollout, horizontal
autoscaling, service mesh integration, and declarative reconciliation of a
production estate. Overpass runs on one machine, deploys by restarting a
container, and scales by editing a `replicas` line.

What Kubernetes would cost is concrete and immediate: a cluster provisioning
step before anything works, manifests or Helm templates for nine components,
image loading into the cluster's registry on every change, ingress configuration
to reach the frontend, and a debugging loop that runs through `kubectl logs`
instead of the terminal already in front of you. On the five-minute cold-start
budget, cluster creation alone is a meaningful fraction.

Compose gives us dependency ordering with health-gated `depends_on`, a shared
network with DNS by service name, volumes for Postgres and NATS persistence,
per-service resource limits when we need them for load-test realism, and
`docker compose up --build <service>` as a two-second iteration loop.

**What the README will say, verbatim in spirit:** production would need
Kubernetes or an equivalent — for multi-node scheduling, rolling deploys, and
autoscaling of `feasibility-service`, which is the component whose statelessness
makes it the natural autoscaling target. Compose is chosen because the artifact
being delivered is a reviewable system, not a production estate. Naming the
limitation is part of the decision.

### Consequences

**Good**

- One command, no prerequisites beyond Docker, and the cold-start budget is
  comfortably met.
- The iteration loop during development is seconds, not minutes.
- Logs are in the terminal. Debugging does not route through a control plane.
- Nine components of orchestration configuration fit in one readable file that a
  reviewer can skim in a minute — which matters, because the reviewer is reading
  the repo as closely as they are running it.

**Bad**

- **No production path.** Compose does not do rolling deploys, health-gated
  rollout, autoscaling, or multi-node scheduling. If this system needed to
  actually serve customers, this decision would be superseded immediately, and
  that successor ADR is the honest shape of the answer.
- Single machine means we cannot demonstrate behaviour under node failure, only
  under process failure. The M3 chaos tests kill containers, which is a strictly
  weaker test than killing nodes.
- Resource limits under Compose are less precise than Kubernetes requests and
  limits, so load-test numbers carry a "on this hardware" caveat. That caveat is
  stated in `docs/performance.md` rather than glossed over.
- `docker compose up --scale` gives us horizontal scaling of stateless services
  for demonstration purposes, but with no scheduling intelligence behind it.

**Neutral**

- The services themselves are unaffected. They are twelve-factor: configuration
  by environment, logs to stdout, no local state. Moving them to Kubernetes later
  is a packaging change, not a code change — which is the property that makes
  this decision cheap to reverse.

### Confirmation

- CI runs `docker compose up` on a clean runner and asserts the full stack is
  healthy and seeded within five minutes. This is the requirement, so it is a
  gate, not a hope.
- The decision is wrong the moment any of these become true: we need more than
  one machine, we need zero-downtime deploys, or we need `feasibility-service` to
  autoscale on queue depth rather than on a hand-edited replica count.

## Pros and cons of the options

### Option 1 — Docker Compose (chosen)

- Good, because it is the smallest thing that satisfies the actual requirement.
- Good, because the iteration loop and the debugging story are both immediate.
- Bad, because there is genuinely no production path, as stated above.

### Option 2 — Kubernetes via kind/k3d

- Good, because it demonstrates production-shaped skills — manifests, probes,
  resource governance, autoscaling — that are relevant to the role.
- Good, because the M3 chaos tests could be richer: pod eviction and node
  pressure are more interesting failure modes than `docker kill`.
- Bad, because the reviewer's setup cost rises from "have Docker" to "have
  Docker, kind, kubectl, and helm, and have them working together". Every
  additional prerequisite is a place the demo dies on someone else's laptop.
- Bad, because cluster creation plus image loading is a substantial share of the
  five-minute budget before a single line of our code has run.
- Bad, because the development loop gets materially slower, and this project's
  scarcest resource is iteration time.

### Option 3 — Compose to run, Kubernetes manifests as an unrun artifact

- Good, because it appears to deliver both the fast demo and the production
  story.
- Bad, because manifests that are never applied are manifests that do not work.
  Untested infrastructure code is a liability that looks like an asset — and a
  reviewer who runs `kubectl apply` and watches it fail has learned something
  worse about the repo than if the manifests had never existed.
- Bad, because maintaining two orchestration definitions doubles the surface for
  configuration drift.
- **This option is genuinely tempting and is rejected on the "untested = broken"
  principle.** If Kubernetes support is ever added, it gets a CI job that
  actually deploys and smoke-tests it, or it does not get added.

### Option 4 — Nomad or similar

- Good, because it is meaningfully lighter than Kubernetes while still being a
  real scheduler with real production semantics.
- Bad, because it is still a cluster to provision and a prerequisite to install,
  so it loses on the dominant driver.
- Bad, because it is less familiar to reviewers than either alternative, so it
  costs explanation without buying capability we need.

## More information

- Component count driving the footprint concern:
  [ADR-0002](0002-nats-jetstream-over-kafka-rabbitmq.md),
  [ADR-0004](0004-postgresql-jsonb-over-document-store.md)
- Cold-start gate: `.github/workflows/` (M0), `docs/performance.md` (M3)
