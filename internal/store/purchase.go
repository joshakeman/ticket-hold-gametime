package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Order struct {
	ID         string    `json:"order_id"`
	HoldID     string    `json:"hold_id"`
	SeatIDs    []int64   `json:"seat_ids"`
	TotalCents int64     `json:"total_cents"`
	CreatedAt  time.Time `json:"created_at"`
}

// Purchase converts a hold into an order. replayed is true when the same
// idempotency key already produced this order and it is returned unchanged.
func (s *Store) Purchase(ctx context.Context, holdID, buyerID, idempotencyKey string) (o Order, replayed bool, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		o, replayed = Order{}, false

		h, err := lockHold(ctx, tx, holdID)
		if err != nil {
			return err
		}
		if h.buyerID != buyerID {
			return ErrForbidden
		}

		// The hold row lock means an existing order is always fully committed.
		var existingKey string
		err = tx.QueryRow(ctx, `
			SELECT id::text, total_cents, idempotency_key, created_at FROM orders WHERE hold_id = $1`, holdID).
			Scan(&o.ID, &o.TotalCents, &existingKey, &o.CreatedAt)
		if err == nil {
			if existingKey != idempotencyKey {
				return ErrAlreadyPurchased
			}
			o.HoldID = holdID
			rows, err := tx.Query(ctx, `SELECT id FROM seats WHERE order_id = $1 ORDER BY id`, o.ID)
			if err != nil {
				return err
			}
			if o.SeatIDs, err = collectIDs(rows); err != nil {
				return err
			}
			replayed = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		o = Order{}

		// Compare-and-set: claim this hold's seats only if the hold is still
		// live. If it lapsed and another buyer took a seat, that seat's hold_id
		// no longer matches and the row count falls short.
		orderID := uuid.NewString()
		rows, err := tx.Query(ctx, `
			UPDATE seats SET order_id = $2, hold_id = NULL, hold_expires_at = NULL
			WHERE hold_id = $1 AND hold_expires_at > now() AND order_id IS NULL
			RETURNING id, price_cents`, holdID, orderID)
		if err != nil {
			return err
		}
		var total int64
		var ids []int64
		for rows.Next() {
			var id int64
			var price int32
			if err := rows.Scan(&id, &price); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
			total += int64(price)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) != h.seatCount {
			if h.expired {
				return ErrHoldExpired
			}
			return ErrSeatsLost
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO orders (id, hold_id, buyer_id, total_cents, idempotency_key)
			VALUES ($1, $2, $3, $4, $5) RETURNING created_at`,
			orderID, holdID, buyerID, total, idempotencyKey).Scan(&o.CreatedAt); err != nil {
			if isUniqueViolation(err) {
				return ErrIdempotencyKeyReuse
			}
			return err
		}
		o.ID, o.HoldID, o.TotalCents = orderID, holdID, total
		o.SeatIDs = sortedUnique(ids)
		return nil
	})
	if err != nil {
		return Order{}, false, err
	}
	return o, replayed, nil
}
