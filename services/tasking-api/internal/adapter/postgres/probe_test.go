package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

type stubRow struct {
	value int
	err   error
}

func (r stubRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if p, ok := dest[0].(*int); ok {
		*p = r.value
	}
	return nil
}

type stubDB struct{ row pgx.Row }

func (s stubDB) QueryRow(context.Context, string, ...any) pgx.Row { return s.row }

func TestCheckPassesWhenTheQueryReturnsOne(t *testing.T) {
	if err := newProbeWith(stubDB{stubRow{value: 1}}).Check(t.Context()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckFailsWhenTheQueryErrors(t *testing.T) {
	// A refused connection is the case readiness exists for.
	err := newProbeWith(stubDB{stubRow{err: errors.New("connection refused")}}).Check(t.Context())
	if err == nil {
		t.Fatal("a failed query reported the database as usable")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("the cause was lost: %v", err)
	}
}

func TestCheckFailsWhenTheAnswerIsWrong(t *testing.T) {
	// A backend that answers, but wrongly, is not a usable backend. This is the
	// case a bare Ping would sail straight through.
	err := newProbeWith(stubDB{stubRow{value: 7}}).Check(t.Context())
	if err == nil {
		t.Fatal("SELECT 1 returning 7 was accepted")
	}
}

func TestNameIdentifiesTheDependency(t *testing.T) {
	// The readiness response keys its checks on this, so a customer-visible
	// payload depends on the string.
	if got := newProbeWith(stubDB{stubRow{value: 1}}).Name(); got != "postgres" {
		t.Fatalf("got %q", got)
	}
}
