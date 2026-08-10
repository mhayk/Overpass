# 0020 — Browser origins are an explicit allow-list, in the services

- **Status:** accepted
- **Date:** 2026-08-10
- **Deciders:** Mhayk Whandson

## Context and problem statement

The web app runs on `:3000`, tasking-api on `:8080`, plan-gateway on `:8083`.
Every read the UI makes and the one write it makes are therefore cross-origin,
and neither service sent a single CORS header. The browser fetched each
response, received it, and then refused to hand it to the page.

Nothing reported this. Health probes were green, `curl` returned data, every Go
and TypeScript unit test passed, and the UI rendered an empty list. The evidence
existed only in a browser console — which is to say, nowhere anyone was looking.
Four M4 issues were closed against a frontend that had never successfully read
from either service in a browser. The first Playwright run found it in one
assertion.

The question: **how do the two services decide which browsers may read them, and
where does that decision live?**

## Decision drivers

- **The failure mode is silence.** A missing CORS header produces no server
  error, no log line, no metric. Whatever is chosen must fail loudly at startup
  instead, because it will not fail visibly at runtime.
- **Two services, one answer.** A browser permitted to POST to tasking-api but
  refused by plan-gateway's preflight is a bug that presents as a network
  problem. The two must not be able to drift.
- **No new dependency.** `github.com/go-chi/cors` would buy roughly seventy
  lines and cost a dependency on a security-relevant path, in a repo where every
  line has to be explainable to a hiring manager.
- **This is not authentication.** CORS constrains what a *page* may read. It
  does nothing to a client that is not a browser, and a design that implies
  otherwise is worse than none.

## Considered options

1. **A shared middleware with an explicit allow-list, wired into both
   services.** One module, one allow-list format, one environment variable.
2. **A Next.js rewrite proxying `/api/*` to both services.** Makes every call
   same-origin, so no CORS at all.
3. **`Access-Control-Allow-Origin: *` on both services.** One header, done.

## Decision outcome

**Option 1.** `lib/go/httpx`, a fourth shared module beside `consume` and
`telemetry`, read from `CORS_ALLOWED_ORIGINS` by both services.

Option 3 was rejected outright. A wildcard is a standing statement that any page
on the internet may read this API, and it cannot be combined with credentials —
so the day either service grows authentication, it becomes a browser-only outage
whose cause is a header set two years earlier.

Option 2 is genuinely attractive and is what a production deployment behind one
domain would do. It was rejected here because it hides the boundary the project
is meant to demonstrate: the read side and the write side are separate services
with separate scaling and separate failure modes, and a proxy that makes them
look like one origin makes them look like one system. It also puts the Next.js
server on the path of the SSE stream, which is a second place for backpressure
to go wrong. Both services still accept an empty allow-list, so this option
remains available without a code change.

Within option 1, four positions worth stating:

- **The origin is echoed, never wildcarded**, and `Vary: Origin` is set on every
  request carrying one — including the refusals. The response differs by origin;
  a shared cache that does not know that will serve one origin's permission to
  another, and that failure appears only behind a proxy.
- **`Access-Control-Allow-Credentials` is not set.** These APIs authenticate
  nothing. Setting it "in case" widens what a hostile page can do with a session
  the moment one exists.
- **`Idempotency-Replayed` is explicitly exposed.** Cross-origin, a page reads
  only the CORS-safelisted response headers; everything else comes back as
  `null`. The client turns that header into a boolean, where `null` and `false`
  are the same value — so a correctly de-duplicated resubmission would have been
  reported to the user as a brand new request. This is the same silent-wrongness
  shape as the original bug, one layer down.
- **Malformed origins are refused at startup.** `http://localhost:3000/` is what
  a person copies out of an address bar, is not an origin, and matches nothing.
  Accepting it yields a service that starts cleanly and blocks the exact origin
  its operator believes they allowed.

## Consequences

**Good.** One allow-list, one format, one variable, refused early when wrong.
No new dependency. Non-browser clients are unaffected, which keeps the
middleware from being mistaken for an access control.

**Bad.** A third shared module to keep in step, and a fourth `replace` directive
in each service's `go.mod`. Adding a deployment origin now requires an
environment change rather than being free.

**Also.** Two gaps closed alongside it. `lib/go/consume` and `lib/go/telemetry`
were absent from `make test-go` and `make lint-go`, so their tests had never run
under `make test`; `GO_LIBS` now covers all three. And the Playwright suite fails
any test in which the page could not reach gateway or tasking-api — asserting on
failed requests rather than console text, because a request to our own API has no
benign failure while a console allow-list rots.

**The rule this reinforces.** Reading is not verification, and neither is
`curl`. The contract these services publish includes being readable by the
client they were built for, and only a browser can check that.
