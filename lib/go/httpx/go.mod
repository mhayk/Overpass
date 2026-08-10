// lib/go/httpx: HTTP middleware shared by the two services a browser talks to.
//
// Its own module for lib/go/consume's and lib/go/telemetry's reasons: it is
// shared by services that deploy separately, and it arrives through a replace
// directive so there is no publishing step and no version skew.
//
// It holds exactly one thing today — the CORS allow-list — and that is
// deliberate. An allow-list is a security decision, and two copies of a
// security decision is one copy that eventually stops matching the other.
//
// No dependencies. Everything here is net/http and the standard library, which
// is why this module has no require block.
module github.com/mhayk/overpass/lib/go/httpx

go 1.25.0
