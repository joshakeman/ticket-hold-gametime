package checkout

import (
	"context"
	"errors"
	"slices"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/josh/ticket-hold-gametime/internal/store"
)

// Payments is the payment provider. Charge must be idempotent on key: the
// same key returns the original charge instead of charging again, because
// Temporal retries an activity whose result was lost.
type Payments interface {
	Charge(ctx context.Context, key, buyerID string, amountCents int64, paymentToken string) (chargeID string, err error)
	Refund(ctx context.Context, chargeID string) error
}

var ErrCardDeclined = errors.New("card declined")

// Activities do the I/O for CheckoutWorkflow.
type Activities struct {
	Store    *store.Store
	Payments Payments
}

type HoldInfo struct {
	HoldID    string    `json:"hold_id"`
	SeatIDs   []int64   `json:"seat_ids"`
	ExpiresAt time.Time `json:"expires_at"`
	// ExpiresIn is the hold's remaining time as computed by Postgres. The
	// workflow's timer uses this duration rather than ExpiresAt, so clock
	// skew between Postgres and Temporal can't make it fire early or late.
	ExpiresIn time.Duration `json:"expires_in"`
	// TotalCents is the price of the held seats, read from the seats table.
	TotalCents int64 `json:"total_cents"`
}

type ChargeInput struct {
	IdempotencyKey string `json:"idempotency_key"`
	BuyerID        string `json:"buyer_id"`
	AmountCents    int64  `json:"amount_cents"`
	PaymentToken   string `json:"payment_token"`
}

type PurchaseInput struct {
	HoldID         string `json:"hold_id"`
	BuyerID        string `json:"buyer_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

type ReleaseInput struct {
	HoldID  string `json:"hold_id"`
	BuyerID string `json:"buyer_id"`
}

func (a *Activities) CreateHold(ctx context.Context, in Input) (HoldInfo, error) {
	h, err := a.Store.CreateHold(ctx, in.EventID, in.BuyerID, in.SeatIDs)
	if err != nil {
		return HoldInfo{}, businessError(err)
	}
	seats, err := a.Store.ListSeats(ctx, in.EventID)
	if err != nil {
		return HoldInfo{}, err
	}
	var total int64
	for _, s := range seats {
		if slices.Contains(h.SeatIDs, s.ID) {
			total += int64(s.PriceCents)
		}
	}
	return HoldInfo{HoldID: h.ID, SeatIDs: h.SeatIDs, ExpiresAt: h.ExpiresAt, ExpiresIn: h.ExpiresIn, TotalCents: total}, nil
}

func (a *Activities) ChargePayment(ctx context.Context, in ChargeInput) (string, error) {
	id, err := a.Payments.Charge(ctx, in.IdempotencyKey, in.BuyerID, in.AmountCents, in.PaymentToken)
	return id, businessError(err)
}

func (a *Activities) RefundPayment(ctx context.Context, chargeID string) error {
	return a.Payments.Refund(ctx, chargeID)
}

func (a *Activities) Purchase(ctx context.Context, in PurchaseInput) (store.Order, error) {
	o, _, err := a.Store.Purchase(ctx, in.HoldID, in.BuyerID, in.IdempotencyKey)
	return o, businessError(err)
}

// ReleaseHold is safe to retry: releasing twice is a no-op.
func (a *Activities) ReleaseHold(ctx context.Context, in ReleaseInput) error {
	return businessError(a.Store.Release(ctx, in.HoldID, in.BuyerID))
}

// businessError marks errors that retrying can't fix as non-retryable, so
// Temporal only retries transient failures such as a dropped connection.
func businessError(err error) error {
	_, unavailable := errors.AsType[*store.UnavailableError](err)
	_, unknown := errors.AsType[*store.UnknownSeatsError](err)
	switch {
	case err == nil:
		return nil
	case unavailable, unknown,
		errors.Is(err, store.ErrEventNotFound),
		errors.Is(err, store.ErrHoldNotFound),
		errors.Is(err, store.ErrForbidden),
		errors.Is(err, store.ErrHoldExpired),
		errors.Is(err, store.ErrAlreadyPurchased),
		errors.Is(err, store.ErrSeatsLost),
		errors.Is(err, store.ErrIdempotencyKeyReuse),
		errors.Is(err, ErrCardDeclined):
		return temporal.NewNonRetryableApplicationError(err.Error(), "BusinessError", err)
	default:
		return err
	}
}
