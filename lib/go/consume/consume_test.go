package consume_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhayk/overpass/lib/go/consume"
)

// The ledger tests run against a real Postgres, because every claim is about
// transaction arbitration — the thing a fake cannot be wrong about the way the
// real one can.

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("OVERPASS_TEST_DSN")
	if dsn == "" {
		t.Skip("set OVERPASS_TEST_DSN to run the ledger tests")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(p.Close)
	// The module owns no schema; the test provides the table shape every
	// service's ledger shares.
	_, err = p.Exec(context.Background(), `
		CREATE SCHEMA IF NOT EXISTS consume_test;
		CREATE TABLE IF NOT EXISTS consume_test.processed_events (
			consumer     text        NOT NULL,
			event_id     uuid        NOT NULL,
			processed_at timestamptz NOT NULL DEFAULT now(),
			PRIMARY KEY (consumer, event_id)
		)`)
	if err != nil {
		t.Fatalf("creating the test ledger: %v", err)
	}
	return p
}

func ledger(t *testing.T) consume.Ledger {
	t.Helper()
	l, err := consume.NewLedger("consume_test.processed_events")
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	return l
}

func inTx(t *testing.T, p *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	t.Helper()
	return pgx.BeginFunc(context.Background(), p, fn)
}

func TestATableNameThatIsNotSchemaTableIsRefused(t *testing.T) {
	// The one string this package interpolates into SQL. Anything but a bare
	// schema.table is refused before it can become an injection.
	for _, bad := range []string{
		"processed_events", "a.b.c", "planning.processed_events; DROP TABLE x",
		"Planning.Events", "", "planning.processed-events",
	} {
		if _, err := consume.NewLedger(bad); err == nil {
			t.Errorf("accepted table name %q", bad)
		}
	}
	if _, err := consume.NewLedger("planning.processed_events"); err != nil {
		t.Errorf("refused a legitimate name: %v", err)
	}
}

func TestFirstClaimWinsAndTheSecondIsADuplicate(t *testing.T) {
	p := pool(t)
	l := ledger(t)
	eventID := fmt.Sprintf("%08x-0000-4000-8000-000000000001", time.Now().UnixNano()&0xffffffff)

	var first, second bool
	if err := inTx(t, p, func(tx pgx.Tx) error {
		var err error
		first, err = l.Claim(context.Background(), tx, "worker", eventID)
		return err
	}); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := inTx(t, p, func(tx pgx.Tx) error {
		var err error
		second, err = l.Claim(context.Background(), tx, "worker", eventID)
		return err
	}); err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if !first || second {
		t.Errorf("first=%v second=%v, want true then false", first, second)
	}
}

// The claim dies with the transaction. A rolled-back claim must NOT count, or
// a crash mid-work would mark the event permanently done without it having
// happened — the silent half of the failure the package doc warns about.
func TestARolledBackClaimDoesNotCount(t *testing.T) {
	p := pool(t)
	l := ledger(t)
	eventID := fmt.Sprintf("%08x-0000-4000-8000-000000000002", time.Now().UnixNano()&0xffffffff)

	rollback := fmt.Errorf("the work blew up")
	err := inTx(t, p, func(tx pgx.Tx) error {
		if _, err := l.Claim(context.Background(), tx, "worker", eventID); err != nil {
			return err
		}
		return rollback // the state change failed; everything rolls back
	})
	if err == nil {
		t.Fatal("the failing transaction reported success")
	}

	var claimed bool
	if err := inTx(t, p, func(tx pgx.Tx) error {
		var err error
		claimed, err = l.Claim(context.Background(), tx, "worker", eventID)
		return err
	}); err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if !claimed {
		t.Fatal("the event stayed claimed after its transaction rolled back; the redelivery would be absorbed as a duplicate and the work never lands")
	}
}

func TestTheLedgerIsPartitionedByConsumer(t *testing.T) {
	p := pool(t)
	l := ledger(t)
	eventID := fmt.Sprintf("%08x-0000-4000-8000-000000000003", time.Now().UnixNano()&0xffffffff)

	for _, consumer := range []string{"worker-a", "worker-b"} {
		var claimed bool
		if err := inTx(t, p, func(tx pgx.Tx) error {
			var err error
			claimed, err = l.Claim(context.Background(), tx, consumer, eventID)
			return err
		}); err != nil {
			t.Fatalf("%s: %v", consumer, err)
		}
		if !claimed {
			t.Errorf("%s was refused; the ledger is keyed on event_id alone and consumers mask each other", consumer)
		}
	}
}

func TestAnEmptyEventIDIsRefused(t *testing.T) {
	p := pool(t)
	l := ledger(t)
	err := inTx(t, p, func(tx pgx.Tx) error {
		_, err := l.Claim(context.Background(), tx, "worker", "")
		return err
	})
	if err == nil {
		t.Fatal("an empty event id entered the ledger; every such message would look like a redelivery of the first")
	}
}

// THE GUARD THAT IS THE POINT OF CLEANUP: retention inside the redelivery
// horizon is refused, because deleting a row the broker can still redeliver
// against reprocesses the redelivery as new — a silent correctness bug that
// only appears under load.
func TestCleanupRefusesRetentionInsideTheRedeliveryHorizon(t *testing.T) {
	p := pool(t)
	l := ledger(t)

	// max_deliver 5 × ack_wait 120s × 2 = 20m horizon. 10 minutes is inside it.
	_, err := l.Cleanup(context.Background(), p, 10*time.Minute, 5, 120*time.Second)
	if err == nil {
		t.Fatal("a retention inside the redelivery horizon was accepted")
	}
	if !strings.Contains(err.Error(), "redelivery horizon") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
}

func TestCleanupDeletesOldRowsAndKeepsFreshOnes(t *testing.T) {
	p := pool(t)
	l := ledger(t)
	ctx := context.Background()

	old := fmt.Sprintf("%08x-0000-4000-8000-00000000000a", time.Now().UnixNano()&0xffffffff)
	fresh := fmt.Sprintf("%08x-0000-4000-8000-00000000000b", time.Now().UnixNano()&0xffffffff)
	if _, err := p.Exec(ctx, `
		INSERT INTO consume_test.processed_events (consumer, event_id, processed_at)
		VALUES ('cleaner', $1, now() - interval '48 hours'),
		       ('cleaner', $2, now())`, old, fresh); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	deleted, err := l.Cleanup(ctx, p, 24*time.Hour, 5, 120*time.Second)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if deleted < 1 {
		t.Errorf("deleted %d rows, want at least the 48-hour-old one", deleted)
	}

	var oldRows, freshRows int
	if err := p.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE event_id = $1), count(*) FILTER (WHERE event_id = $2)
		 FROM consume_test.processed_events WHERE consumer='cleaner'`,
		old, fresh).Scan(&oldRows, &freshRows); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if oldRows != 0 {
		t.Error("the stale row survived")
	}
	if freshRows != 1 {
		t.Error("a fresh row was deleted; its redelivery would be reprocessed as new")
	}
}
