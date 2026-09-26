package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type Hold struct {
	ID        string    `json:"hold_id"`
	EventID   int64     `json:"event_id"`
	SeatIDs   []int64   `json:"seat_ids"`
	ExpiresAt time.Time `json:"expires_at"`
	// ExpiresIn is how long the hold has left, computed by Postgres so that
	// callers never compare our timestamps with their own clock. Only set by
	// CreateHold.
	ExpiresIn time.Duration `json:"-"`
	// Status is "active", "expired" or "purchased". Only set by GetHold.
	Status string `json:"status,omitempty"`
}

type holdRow struct {
	id        string
	eventID   int64
	buyerID   string
	seatCount int
	expiresAt time.Time
	expired   bool
}

// lockHold row-locks the hold, which serializes concurrent purchase/release
// calls on the same hold.
func lockHold(ctx context.Context, tx pgx.Tx, id string) (holdRow, error) {
	var h holdRow
	err := tx.QueryRow(ctx, `
		SELECT id::text, event_id, buyer_id, seat_count, expires_at, expires_at <= now()
		FROM holds WHERE id = $1 FOR UPDATE`, id).
		Scan(&h.id, &h.eventID, &h.buyerID, &h.seatCount, &h.expiresAt, &h.expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return h, ErrHoldNotFound
	}
	return h, err
}

// CreateHold holds every requested seat or none of them.
func (s *Store) CreateHold(ctx context.Context, eventID int64, buyerID string, seatIDs []int64) (Hold, error) {
	ids := sortedUnique(seatIDs)
	var out Hold

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		out = Hold{}

		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE id = $1)`, eventID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrEventNotFound
		}

		// Lock the rows in id order so overlapping requests cannot deadlock.
		// This statement only orders the locking; the UPDATE below is what
		// decides who wins, and it runs on a fresh snapshot taken after the
		// locks are acquired.
		rows, err := tx.Query(ctx,
			`SELECT id FROM seats WHERE event_id = $1 AND id = ANY($2) ORDER BY id FOR UPDATE`, eventID, ids)
		if err != nil {
			return err
		}
		found, err := collectIDs(rows)
		if err != nil {
			return err
		}
		if unknown := missing(ids, found); len(unknown) > 0 {
			return &UnknownSeatsError{SeatIDs: unknown}
		}

		var holdID string
		var expiresAt time.Time
		var expiresInSecs float64
		if err := tx.QueryRow(ctx, `
			INSERT INTO holds (event_id, buyer_id, seat_count, expires_at)
			VALUES ($1, $2, $3, now() + make_interval(secs => $4::float8))
			RETURNING id::text, expires_at, extract(epoch FROM expires_at - now())::float8`,
			eventID, buyerID, len(ids), s.holdDuration.Seconds()).Scan(&holdID, &expiresAt, &expiresInSecs); err != nil {
			return err
		}

		// Compare-and-set: only seats that are unsold and unheld (or whose hold
		// has expired) are taken. Any shortfall means someone else has them.
		rows, err = tx.Query(ctx, `
			UPDATE seats SET hold_id = $1, hold_expires_at = $2
			WHERE event_id = $3 AND id = ANY($4)
			  AND order_id IS NULL
			  AND (hold_id IS NULL OR hold_expires_at <= now())
			RETURNING id`, holdID, expiresAt, eventID, ids)
		if err != nil {
			return err
		}
		taken, err := collectIDs(rows)
		if err != nil {
			return err
		}
		if len(taken) != len(ids) {
			return &UnavailableError{SeatIDs: missing(ids, taken)}
		}

		out = Hold{
			ID: holdID, EventID: eventID, SeatIDs: ids,
			ExpiresAt: expiresAt,
			ExpiresIn: time.Duration(expiresInSecs * float64(time.Second)),
		}
		return nil
	})
	return out, err
}

// GetHold returns the hold's current state. Only the owning buyer may read it.
func (s *Store) GetHold(ctx context.Context, holdID, buyerID string) (Hold, error) {
	var h Hold
	var owner string
	var expired, purchased bool
	err := s.pool.QueryRow(ctx, `
		SELECT h.id::text, h.event_id, h.buyer_id, h.expires_at, h.expires_at <= now(),
		       EXISTS (SELECT 1 FROM orders o WHERE o.hold_id = h.id)
		FROM holds h WHERE h.id = $1`, holdID).
		Scan(&h.ID, &h.EventID, &owner, &h.ExpiresAt, &expired, &purchased)
	if errors.Is(err, pgx.ErrNoRows) {
		return Hold{}, ErrHoldNotFound
	}
	if err != nil {
		return Hold{}, err
	}
	if owner != buyerID {
		return Hold{}, ErrForbidden
	}

	switch {
	case purchased:
		h.Status = "purchased"
	case expired:
		h.Status = "expired"
	default:
		h.Status = "active"
	}
	if h.Status == "expired" {
		h.SeatIDs = []int64{}
		return h, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id FROM seats
		WHERE hold_id = $1 OR order_id = (SELECT id FROM orders WHERE hold_id = $1)
		ORDER BY id`, holdID)
	if err != nil {
		return Hold{}, err
	}
	if h.SeatIDs, err = collectIDs(rows); err != nil {
		return Hold{}, err
	}
	if h.SeatIDs == nil {
		h.SeatIDs = []int64{}
	}
	return h, nil
}

// Release gives the hold's seats back immediately. Releasing twice is fine.
func (s *Store) Release(ctx context.Context, holdID, buyerID string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		h, err := lockHold(ctx, tx, holdID)
		if err != nil {
			return err
		}
		if h.buyerID != buyerID {
			return ErrForbidden
		}
		var purchased bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM orders WHERE hold_id = $1)`, holdID).Scan(&purchased); err != nil {
			return err
		}
		if purchased {
			return ErrAlreadyPurchased
		}
		if _, err := tx.Exec(ctx, `
			UPDATE seats SET hold_id = NULL, hold_expires_at = NULL
			WHERE hold_id = $1 AND order_id IS NULL`, holdID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE holds SET expires_at = least(expires_at, now()) WHERE id = $1`, holdID)
		return err
	})
}
