package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Ingress with the database out of reach.
//
// The failure this covers is not a crash. It is a service that stays up,
// answers its health probes, and holds every submission open forever because
// the connection it needs is never free. Every dashboard is green, the request
// count simply stops, and the customer's client times out with no idea whether
// the request was accepted — which is the one thing tasking-api must never
// leave ambiguous.
//
// The contract under test: a valid request that could not be stored is REFUSED
// with 503, never 202. A refusal is recoverable; a hang and an unwarranted
// acceptance are not.

// withPoolLimit returns the harness DSN with a connection cap, so the pool can
// be exhausted deliberately rather than by brute force.
func withPoolLimit(dsn string, conns int) string {
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return dsn + separator + "pool_max_conns=" + itoa(conns)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestIngressRefusesRatherThanHangsWhenTheDatabaseIsUnreachable(t *testing.T) {
	ctx := t.Context()

	// The customer the body names has to exist, or the recovery half of this
	// test measures a foreign key rather than a reachable database — which is
	// exactly what it did on the first run: 503 while locked, 503 afterwards,
	// for two completely different reasons.
	seedCustomer(t)

	// A pool of one, so a single blocked statement is the whole capacity. The
	// alternative — opening hundreds of connections to starve a default-sized
	// pool — would test the machine's file descriptor limit.
	api, err := start(env.taskingAPIBin, "tasking-api", map[string]string{
		"DATABASE_URL":   withPoolLimit(env.dsn, 1),
		"SUBMIT_TIMEOUT": "2",
	})
	if err != nil {
		t.Fatalf("starting tasking-api: %v", err)
	}
	t.Cleanup(func() {
		if killErr := api.Kill(); killErr != nil {
			t.Errorf("killing tasking-api: %v", killErr)
		}
	})

	// Its own connection, held open with an exclusive lock on the table every
	// submission must write. The API's one connection blocks the moment it
	// tries, which is what an exhausted pool feels like from the handler:
	// the work never starts.
	blocker, err := pgx.Connect(ctx, env.dsn)
	if err != nil {
		t.Fatalf("connecting the blocker: %v", err)
	}
	defer blocker.Close(context.Background()) //nolint:errcheck // test teardown

	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning the blocking transaction: %v", err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE tasking.tasking_requests IN EXCLUSIVE MODE`); err != nil {
		t.Fatalf("locking the table: %v", err)
	}

	body := chaosRequestBody(t)
	status, elapsed := submitStatus(t, api, uuid.NewString(), body)

	if status == http.StatusAccepted || status == http.StatusOK {
		t.Fatalf("the API accepted a request it could not store (status %d) — "+
			"the customer believes an image is coming and nothing in the system knows about it", status)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d after %s, want 503 — a retryable refusal is the only honest answer here", status, elapsed)
	}
	// The refusal has to be prompt. A 503 that arrives after five minutes is a
	// hang with a status code attached, and the client has long since given up.
	if elapsed > 30*time.Second {
		t.Errorf("the refusal took %s; ingress is queueing rather than shedding", elapsed)
	}

	// Nothing half-written. The submission and its outbox row commit together,
	// so a refused request must leave neither.
	if got := rowCount(t, `SELECT count(*) FROM tasking.tasking_requests WHERE customer_id = $1
	                        AND submitted_at > now() - interval '2 minutes'`, traceCustomerID); got != 0 {
		t.Errorf("%d request rows written by a submission that was refused", got)
	}

	// And it recovers on its own once the database is reachable again — no
	// restart, no intervention. A service that needs a kick after a blip is a
	// service that will need one at three in the morning.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("releasing the lock: %v", err)
	}

	lastCode := 0
	if !waitFor(30*time.Second, func() bool {
		lastCode, _ = submitStatus(t, api, uuid.NewString(), chaosRequestBody(t))
		return lastCode == http.StatusAccepted || lastCode == http.StatusOK
	}) {
		t.Fatalf("ingress never recovered after the database became reachable again; last status %d\n%s",
			lastCode, api.logs.String())
	}
}

// submitStatus posts one request and reports the status and how long it took.
func submitStatus(t *testing.T, api *service, key string, body []byte) (int, time.Duration) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		api.baseURL+"/v1/tasking-requests", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)

	// Generous relative to the service's own 2s budget: this timeout exists to
	// stop the test hanging forever, not to define the behaviour. If it is what
	// fires, the service failed to answer and the assertions below say so.
	start := time.Now()
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("submitting: %v (after %s)", err, elapsed)
	}
	defer resp.Body.Close() //nolint:errcheck // status is all this needs
	return resp.StatusCode, elapsed
}
