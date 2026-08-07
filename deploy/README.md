# Deployment configuration

Everything here is applied by `docker compose up`. One machine, nine containers
— see [ADR-0005](../docs/decisions/0005-docker-compose-over-kubernetes.md) for
why this is not Kubernetes, and what production would actually need.

| Directory | Contents |
| --- | --- |
| `nats/` | JetStream stream and consumer definitions, applied by an init container |
| `postgres/` | Extension bootstrap (PostGIS), tuning |
| `otel/` | Collector pipeline configuration |
| `prometheus/` | Scrape config and alert rules |
| `grafana/` | Provisioned datasources and committed dashboard JSON |

NATS topology is declared here rather than created lazily from application code,
so it is reviewable in a pull request and two services cannot race to create the
same stream with different settings. The contract it implements is
[`contracts/nats/topology.md`](../contracts/nats/topology.md).
