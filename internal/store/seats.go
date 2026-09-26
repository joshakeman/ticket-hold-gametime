package store

import "context"

type Seat struct {
	ID         int64  `json:"id"`
	Label      string `json:"label"`
	PriceCents int32  `json:"price_cents"`
	// Status is "available", "held" or "sold".
	Status string `json:"status"`
}

func (s *Store) ListSeats(ctx context.Context, eventID int64) ([]Seat, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE id = $1)`, eventID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrEventNotFound
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, label, price_cents,
		       CASE WHEN order_id IS NOT NULL THEN 'sold'
		            WHEN hold_id IS NOT NULL AND hold_expires_at > now() THEN 'held'
		            ELSE 'available' END
		FROM seats WHERE event_id = $1 ORDER BY id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seats := []Seat{}
	for rows.Next() {
		var st Seat
		if err := rows.Scan(&st.ID, &st.Label, &st.PriceCents, &st.Status); err != nil {
			return nil, err
		}
		seats = append(seats, st)
	}
	return seats, rows.Err()
}
