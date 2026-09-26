// Package store holds all SQL. Seat state changes are single conditional
// UPDATEs, so Postgres decides the winner when two buyers race for a seat.
package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const DefaultHoldDuration = 10 * time.Minute

var (
	ErrEventNotFound       = errors.New("event not found")
	ErrHoldNotFound        = errors.New("hold not found")
	ErrForbidden           = errors.New("hold belongs to another buyer")
	ErrHoldExpired         = errors.New("hold expired")
	ErrAlreadyPurchased    = errors.New("hold already purchased")
	ErrSeatsLost           = errors.New("held seats are no longer available")
	ErrIdempotencyKeyReuse = errors.New("idempotency key already used for a different hold")
)

// UnknownSeatsError lists requested seats that do not exist in the event.
type UnknownSeatsError struct{ SeatIDs []int64 }

func (e *UnknownSeatsError) Error() string { return fmt.Sprintf("unknown seats: %v", e.SeatIDs) }

// UnavailableError lists requested seats that are sold or held by someone else.
type UnavailableError struct{ SeatIDs []int64 }

func (e *UnavailableError) Error() string { return fmt.Sprintf("seats unavailable: %v", e.SeatIDs) }

type Store struct {
	pool         *pgxpool.Pool
	holdDuration time.Duration
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, holdDuration: DefaultHoldDuration}
}

// inTx runs fn in a READ COMMITTED transaction, retrying once on deadlock or
// serialization failure. fn may run twice, so it must reset any captured state.
func (s *Store) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	var err error
	for range 2 {
		err = s.runTx(ctx, fn)
		if !isRetryable(err) {
			return err
		}
	}
	return err
}

func (s *Store) runTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func isRetryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001")
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// sortedUnique returns ids sorted ascending with duplicates removed.
// De-duplicating keeps a hold's seat_count equal to the rows it claims, and
// sorting gives predictable output. It does not set lock order: that comes
// from ORDER BY id in CreateHold's locking query.
func sortedUnique(ids []int64) []int64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

// missing returns the ids in want that are not in got.
func missing(want, got []int64) []int64 {
	have := make(map[int64]bool, len(got))
	for _, id := range got {
		have[id] = true
	}
	var out []int64
	for _, id := range want {
		if !have[id] {
			out = append(out, id)
		}
	}
	return out
}

func collectIDs(rows pgx.Rows) ([]int64, error) {
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
