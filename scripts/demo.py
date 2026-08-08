"""Submit a scripted, deliberately contested set of tasking requests.

Run via ``scripts/demo.sh`` or ``make demo``.

The scenario is designed to CONTEND. A demo where every request wins shows
nothing about the system that matters — the interesting output is what happens
when four customers want the same satellite over the same water at the same
time, and something has to lose.

**What this shows today, and what it does not.** The planner is M2. So a run
right now demonstrates ingress → outbox → feasibility: requests accepted,
persisted with their idempotency keys, published exactly once, swept against the
real constellation with SGP4, and projected as candidate opportunities the globe
can draw. The de-confliction those candidates feed is the part that arrives with
the planner, and the scenario below is built so that it will contend the moment
it does, rather than needing to be rewritten then.

That claim was false until #131 and is worth stating precisely because of it:
the worker recorded every request as processed and computed nothing, so a demo
run produced ingress and silence.

Idempotent. Every request carries a deterministic Idempotency-Key derived from
the scenario, so a second run replays rather than duplicating — which is worth
demonstrating in its own right, because "submitted twice, accepted once" is a
claim the system makes and a reviewer can check by running this twice.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request
import uuid
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta

GREEN, RED, YELLOW, DIM, RESET = (
    "\033[32m",
    "\033[31m",
    "\033[33m",
    "\033[90m",
    "\033[0m",
)

# A fixed namespace, so the same scenario always produces the same keys.
# Deterministic rather than random: the idempotency claim is only observable if
# the second run presents the SAME key, and a uuid4 per run would quietly turn
# the replay demonstration into four more submissions.
DEMO_NAMESPACE = uuid.UUID("6f9619ff-8b86-d011-b42d-00c04fc964ff")


# The key is derived from the BODY, not just the scenario name.
#
# That is how a correct client behaves, and getting it wrong is instructive: the
# first version keyed on the name alone and computed the window from
# datetime.now(), so the second run sent a different body under the same key and
# the API answered 409 — "this key was used for a different request". The API was
# right and the demo was wrong. Hashing the body means same body, same key,
# clean replay; changed body, new key, new request. Never a spurious conflict.
def _idempotency_key(name: str, body: dict) -> str:
    canonical = json.dumps(body, sort_keys=True, separators=(",", ":"))
    return str(uuid.uuid5(DEMO_NAMESPACE, f"{name}\n{canonical}"))


def _window_anchor(now: datetime) -> datetime:
    """The top of the current UTC hour.

    Anchored rather than relative so two runs a minute apart produce a
    byte-identical body and therefore a replay. An hour later the body
    legitimately differs, the derived key differs with it, and the request is
    accepted as new — which is correct, not a failure: it IS a different
    request, for a different window.
    """
    return now.replace(minute=0, second=0, microsecond=0)


@dataclass(frozen=True)
class Request:
    name: str
    customer_id: str
    target_name: str
    target: dict
    priority_tier: str
    bid_credits: int
    modes: list[str]
    window_hours: int = 24
    why: str = ""
    constraints: dict = field(default_factory=dict)


# The Port of Rotterdam and its approaches. One place, four customers.
#
# Geographic overlap is what creates contention: these targets are close enough
# that one pass can only image some of them, so the planner has to choose. Four
# requests spread over four tiers with different bids is the smallest scenario
# that makes tier, bid and geometry all matter at once.
ROTTERDAM_PORT = {
    "type": "Polygon",
    "coordinates": [
        [[4.02, 51.92], [4.18, 51.92], [4.18, 51.99], [4.02, 51.99], [4.02, 51.92]]
    ],
}
ROTTERDAM_APPROACH = {
    "type": "Polygon",
    "coordinates": [
        [[3.85, 51.95], [4.05, 51.95], [4.05, 52.05], [3.85, 52.05], [3.85, 51.95]]
    ],
}
MAASVLAKTE = {"type": "Point", "coordinates": [4.05, 51.95]}
NORTH_SEA_SPILL = {
    "type": "Polygon",
    "coordinates": [
        [[3.70, 51.98], [3.95, 51.98], [3.95, 52.12], [3.70, 52.12], [3.70, 51.98]]
    ],
}

SCENARIO = [
    Request(
        name="coastguard-spill-response",
        customer_id="nl-coastguard",
        target_name="Suspected oil spill, North Sea",
        target=NORTH_SEA_SPILL,
        priority_tier="GOVERNMENT",
        bid_credits=0,
        modes=["SCAN", "STRIPMAP"],
        window_hours=12,
        # Tier beats bid, and this is the request that proves it: zero credits
        # against a commercial bid of 20000. If the planner ever awards on money
        # alone, this is the request that disappears.
        why="highest tier, zero bid — tier must beat money",
    ),
    Request(
        name="civil-protection-flood-watch",
        customer_id="eu-civil-protection",
        target_name="Flood watch, Rotterdam approaches",
        target=ROTTERDAM_APPROACH,
        priority_tier="CIVIL_PROTECTION",
        bid_credits=2000,
        modes=["SCAN"],
        window_hours=24,
        why="second tier, overlapping geometry with the spill response",
    ),
    Request(
        name="port-berth-survey",
        customer_id="port-authority-nl",
        target_name="Port of Rotterdam berth survey",
        target=ROTTERDAM_PORT,
        priority_tier="COMMERCIAL",
        bid_credits=20000,
        modes=["STRIPMAP", "SPOTLIGHT"],
        window_hours=48,
        # The biggest bid in the scenario, and it should still lose the contested
        # pass. A demo where the largest cheque wins is a demo of a marketplace,
        # not of a tasking system with a fairness model.
        why="largest bid, lower tier — must not outrank the coastguard",
        constraints={
            "look_side": "RIGHT",
            "min_incidence_deg": 20,
            "max_incidence_deg": 45,
        },
    ),
    Request(
        name="acme-maasvlakte-point",
        customer_id="acme-imaging",
        target_name="Maasvlakte container terminal",
        target=MAASVLAKTE,
        priority_tier="BEST_EFFORT",
        bid_credits=500,
        modes=["SPOTLIGHT"],
        window_hours=72,
        # A wide window on the lowest tier. It should lose every contested pass
        # and still eventually win one, which is what ageing is for — and what a
        # demo that only ran once would never show.
        why="lowest tier, widest window — should win only where nobody contends",
    ),
]


def submit(base_url: str, request: Request, now: datetime) -> tuple[int, bool, dict]:
    anchor = _window_anchor(now)
    body = {
        "customer_id": request.customer_id,
        "target_name": request.target_name,
        "target": request.target,
        "window": {
            "start": (anchor + timedelta(hours=1)).isoformat().replace("+00:00", "Z"),
            "end": (anchor + timedelta(hours=1 + request.window_hours))
            .isoformat()
            .replace("+00:00", "Z"),
        },
        "priority_tier": request.priority_tier,
        "bid_credits": request.bid_credits,
        "requested_modes": request.modes,
    }
    if request.constraints:
        body["constraints"] = request.constraints

    payload = json.dumps(body).encode()
    # A fixed local URL from --base-url; nothing here takes a remote scheme.
    http = urllib.request.Request(
        f"{base_url}/v1/tasking-requests",
        data=payload,
        method="POST",
        headers={
            "Content-Type": "application/json",
            "Idempotency-Key": _idempotency_key(request.name, body),
        },
    )
    try:
        with urllib.request.urlopen(http, timeout=30) as response:
            # The API signals a replay with a HEADER, not a status code: both a
            # new acceptance and a replay are 202, because from the caller's
            # point of view the outcome is identical — the request is accepted
            # and will be planned exactly once. Reading the status alone, as the
            # first version did, cannot tell them apart and reported four
            # replays as four new submissions.
            replayed = response.headers.get("Idempotency-Replayed") == "true"
            return response.status, replayed, json.loads(response.read() or b"{}")
    except urllib.error.HTTPError as exc:
        return exc.code, False, json.loads(exc.read() or b"{}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--base-url",
        default=os.getenv("TASKING_API_URL", "http://localhost:8080"),
        help="tasking-api base URL",
    )
    args = parser.parse_args()
    now = datetime.now(UTC)

    print(f"\n{YELLOW}Submitting {len(SCENARIO)} contested requests{RESET}")
    print(f"{DIM}one port, four customers, four tiers — something has to lose{RESET}\n")

    failures = 0
    replays = 0
    for request in SCENARIO:
        status, replay, body = submit(args.base_url, request, now)
        label = f"{request.customer_id} {DIM}{request.priority_tier}, {request.bid_credits} credits{RESET}"

        if status in (200, 201, 202):
            replays += int(replay)
            marker = f"{DIM}(replay){RESET}" if replay else ""
            print(f"  {GREEN}ok{RESET}   {label} {marker}")
            print(f"       {DIM}{request.why}{RESET}")
        elif status == 409:
            # Not a demo failure. The key was reused with a different body,
            # which is precisely what the API promises to refuse — worth showing
            # rather than hiding, because a system that quietly accepted it
            # would be the broken one.
            failures += 1
            print(f"  {RED}409{RESET}  {label}")
            print(
                f"       {DIM}the API refused a reused key with a changed body — "
                f"that is correct behaviour, but this demo should never trigger it{RESET}"
            )
        else:
            failures += 1
            detail = body.get("detail") or body.get("title") or body
            print(f"  {RED}FAIL{RESET} {label}")
            print(f"       {DIM}{status}: {detail}{RESET}")

    print()
    if failures:
        print(f"{RED}{failures} of {len(SCENARIO)} requests were refused{RESET}")
        print(f"{DIM}is the stack up, and has `make seed` run?{RESET}\n")
        return 1

    if replays == len(SCENARIO):
        print(
            f"{GREEN}all {len(SCENARIO)} replayed{RESET} — idempotency held; nothing was duplicated"
        )
    elif replays:
        print(
            f"{GREEN}accepted{RESET} {len(SCENARIO) - replays} new, {replays} replayed"
        )
    else:
        print(f"{GREEN}accepted {len(SCENARIO)} requests{RESET}")

    print(
        f"\n{DIM}Feasibility is sweeping the constellation for access windows now.\n"
        f"The de-confliction these feed arrives with the planner in M2 — the\n"
        f"scenario above is built to contend the moment it does.{RESET}\n"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
