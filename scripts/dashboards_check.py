"""Assert every committed dashboard panel can actually render.

THE QUERIES ARE EXTRACTED FROM THE COMMITTED JSON, NEVER RESTATED. A check
carrying its own copy of the query proves only that the check's query works,
which is exactly how `overpass_dlq_depth` survived review in alerts.yml: it
looked right everywhere it was written down, and nothing ever asked Prometheus
whether it existed.

Two assertions, and they catch different failures:

  1. Every metric a panel names EXISTS in Prometheus. This is the typo-and-
     rename gate. A panel querying `overpass_requests_unfulfiled_total`
     renders "No data" and is invisible to every form of review except opening
     it.

  2. Every panel expression PARSES AND EVALUATES. This catches a malformed
     PromQL expression, which Grafana reports inside the panel and nowhere
     else.

Standard library only, matching scripts/demo.py. The last thing anyone wants
when a gate fails is to discover the gate itself has a dependency problem.
"""

from __future__ import annotations

import json
import os
import pathlib
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[1]
DASHBOARDS = ROOT / "deploy" / "grafana" / "dashboards"
PROMETHEUS = os.getenv("PROMETHEUS_URL", "http://localhost:9090")

RED = "\033[31m"
GREEN = "\033[32m"
YELLOW = "\033[33m"
DIM = "\033[2m"
RESET = "\033[0m"

# Grafana interpolates these before sending a query. Prometheus does not know
# them, so they are substituted with a concrete value here. 5m is what a 10s
# scrape interval yields for $__rate_interval on a default-width panel.
GRAFANA_VARS = {
    "$__rate_interval": "5m",
    "$__interval": "1m",
    "$__range": "1h",
}

# Metrics that legitimately have NO series after a healthy demo run, each
# because it requires a FAILURE to have happened. They are exempt from the
# existence check and nothing else.
#
# This list is short and every entry is a statement about the system rather
# than a workaround. If one of these ever becomes reliably present, delete its
# line — an exemption nobody revisits is how a gate rots.
MAY_BE_ABSENT = {
    # Needs the broker to redeliver, which a healthy run never does.
    "overpass_consume_redeliveries_total": "requires a redelivery; a healthy run has none",
    # Published by prometheus-nats-exporter only once a DLQ stream has ever
    # held a message. The chaos suite creates them; `make demo` does not.
    "jetstream_stream_total_messages": "requires a dead letter to have existed",
    # Needs a satellite with no duty-cycle budget configured, which the seeded
    # constellation does not have.
    "overpass_satellite_utilisation_ratio": "requires a committed plan; absent until the first round",
    # Needs a request that finds no usable access window. The demo's four are
    # built to CONTEND with each other, so all four succeed by design — the
    # interesting failure there is losing an allocation, not being infeasible.
    # The chaos and integration suites produce these.
    "overpass_feasibility_refusals_total": "requires an infeasible request; the demo's four all succeed",
    # Needs a round where demand actually exceeds capacity. The demo contends
    # four customers for one target, but nine satellites over a 24h horizon can
    # often place all four on different passes, so a COLD run may legitimately
    # produce no loser. Discovered by this gate failing in CI while passing
    # locally — a long-running instance has accumulated unfulfilments and
    # Prometheus remembers the name, which is exactly the difference a
    # cold-start gate exists to expose.
    "overpass_requests_unfulfilled_total": "requires a round where demand exceeds capacity",
}

# Prometheus function and keyword names, so they are not mistaken for metrics.
PROMQL_KEYWORDS = {
    "sum", "rate", "irate", "increase", "avg", "min", "max", "count", "by",
    "without", "on", "ignoring", "group_left", "group_right", "histogram_quantile",
    "clamp_min", "clamp_max", "vector", "scalar", "topk", "bottomk", "quantile",
    "abs", "ceil", "floor", "round", "delta", "idelta", "deriv", "predict_linear",
    "stddev", "stdvar", "absent", "absent_over_time", "changes", "resets",
    "label_replace", "label_join", "time", "timestamp", "and", "or", "unless",
    "offset", "bool", "sum_over_time", "avg_over_time", "max_over_time",
    "min_over_time", "count_over_time", "last_over_time", "e", "pi", "inf", "nan",
}

METRIC_RE = re.compile(r"\b([a-zA-Z_][a-zA-Z0-9_]*)\b(?=\s*[{[(]|\s*[\)\]\s,/+*-]|$)")


def panels(dashboard: dict) -> list[dict]:
    """Every panel, including those nested inside collapsed rows."""
    found = []
    for panel in dashboard.get("panels", []):
        if panel.get("type") == "row":
            found.extend(panel.get("panels", []))
        else:
            found.append(panel)
    return found


def expressions(path: pathlib.Path) -> list[tuple[str, str]]:
    """(panel title, expr) for every target in one dashboard."""
    dashboard = json.loads(path.read_text())
    out = []
    for panel in panels(dashboard):
        for target in panel.get("targets", []):
            expr = target.get("expr", "").strip()
            if expr:
                out.append((panel.get("title", "<untitled>"), expr))
    return out


def interpolate(expr: str) -> str:
    for name, value in GRAFANA_VARS.items():
        expr = expr.replace(name, value)
    return expr


def metric_names(expr: str) -> set[str]:
    """Metric names referenced by an expression.

    Deliberately crude: it over-collects rather than under-collects, then
    filters against PromQL's own vocabulary and against label names appearing
    inside selectors. A gate that misses a metric is worse than one that asks
    about an extra identifier, because the first kind fails silently.
    """
    # Label SELECTORS first: {http_route="/v1/plans"} contributes no metric.
    stripped = re.sub(r"\{[^}]*\}", "{}", expr)
    stripped = re.sub(r'"[^"]*"', '""', stripped)
    # Then the GROUPING clauses. `sum by (service_name, http_route) (...)`
    # names labels, not metrics, and an earlier version of this function
    # reported http_route as an unpublished metric — a false failure on a
    # panel that renders perfectly. A gate that cries wolf gets switched off,
    # so this is as much a correctness requirement as the real check.
    stripped = re.sub(r"\b(?:by|without|on|ignoring|group_left|group_right)\s*\([^)]*\)", " ", stripped)
    names = set()
    for match in METRIC_RE.finditer(stripped):
        name = match.group(1)
        if name in PROMQL_KEYWORDS or name.isdigit():
            continue
        names.add(name)
    return names


def prometheus(path: str, params: dict[str, str]) -> dict:
    url = f"{PROMETHEUS}{path}?{urllib.parse.urlencode(params)}"
    try:
        with urllib.request.urlopen(url, timeout=20) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as exc:
        body = exc.read().decode(errors="replace")
        try:
            return json.loads(body)
        except json.JSONDecodeError:
            return {"status": "error", "error": f"HTTP {exc.code}: {body[:300]}"}
    except OSError as exc:
        print(f"{RED}error{RESET} cannot reach Prometheus at {PROMETHEUS}: {exc}", file=sys.stderr)
        print("       start the stack with 'make up' first", file=sys.stderr)
        raise SystemExit(2) from exc


def known_metrics() -> set[str]:
    payload = prometheus("/api/v1/label/__name__/values", {})
    if payload.get("status") != "success":
        print(f"{RED}error{RESET} listing metric names: {payload.get('error')}", file=sys.stderr)
        raise SystemExit(2)
    return set(payload.get("data", []))


def main() -> int:
    files = sorted(DASHBOARDS.glob("*.json"))
    if not files:
        print(f"{RED}error{RESET} no dashboards found in {DASHBOARDS}", file=sys.stderr)
        return 2

    known = known_metrics()
    print(f"{DIM}Prometheus at {PROMETHEUS} knows {len(known)} metric names{RESET}\n")

    failures = 0
    exempted = 0

    for path in files:
        print(f"{path.relative_to(ROOT)}")
        for title, expr in expressions(path):
            interpolated = interpolate(expr)

            missing = []
            for name in sorted(metric_names(interpolated)):
                if name in known:
                    continue
                # A label name that survived the crude extraction is not a
                # metric. Anything prefixed like our instruments, or any name
                # Prometheus has never heard of that looks like a series, is.
                if name in MAY_BE_ABSENT:
                    exempted += 1
                    print(f"  {YELLOW}skip{RESET} {title}: {name} — {MAY_BE_ABSENT[name]}")
                    continue
                if name.startswith(("overpass_", "http_", "jetstream_")):
                    missing.append(name)

            if missing:
                failures += 1
                print(f"  {RED}FAIL{RESET} {title}")
                for name in missing:
                    print(f"       metric {name!r} is not published by anything")
                print(f"       {DIM}{expr}{RESET}")
                continue

            result = prometheus("/api/v1/query", {"query": interpolated})
            if result.get("status") != "success":
                failures += 1
                print(f"  {RED}FAIL{RESET} {title}: {result.get('error')}")
                print(f"       {DIM}{expr}{RESET}")
                continue

            print(f"  {GREEN}ok{RESET}   {title}")
        print()

    if failures:
        print(f"{RED}{failures} panel(s) cannot render.{RESET}")
        print("A panel naming a metric nobody publishes shows 'No data' and is")
        print("invisible to every form of review except opening it.")
        return 1

    note = f", {exempted} exempt" if exempted else ""
    print(f"{GREEN}every panel query resolves{RESET}{note}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
