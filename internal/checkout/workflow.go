// Package checkout runs a purchase as a Temporal workflow: hold the seats,
// wait for the buyer to pay or the hold to expire, charge, then record the
// order. If a later step fails, earlier steps are undone in reverse order
// (a saga).
//
// Workflow code must be deterministic. Temporal rebuilds a workflow's state
// after a crash by replaying its history through this code, so every replay
// has to make the same decisions. That rules out time.Now, randomness, and
// I/O here: use workflow.Now and workflow timers, and do all I/O in
// activities.
package checkout

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/josh/ticket-hold-gametime/internal/store"
)

const (
	TaskQueue = "checkout"
	PaySignal = "pay"
)

// Result statuses.
const (
	StatusPurchased      = "purchased"
	StatusHoldExpired    = "hold_expired"
	StatusPaymentFailed  = "payment_failed"
	StatusPurchaseFailed = "purchase_failed"
)

type Input struct {
	EventID        int64   `json:"event_id"`
	BuyerID        string  `json:"buyer_id"`
	SeatIDs        []int64 `json:"seat_ids"`
	IdempotencyKey string  `json:"idempotency_key"`
}

// PayRequest is the payload of the "pay" signal.
type PayRequest struct {
	PaymentToken string `json:"payment_token"`
}

type Result struct {
	Status     string `json:"status"`
	HoldID     string `json:"hold_id,omitzero"`
	OrderID    string `json:"order_id,omitzero"`
	ChargeID   string `json:"charge_id,omitzero"`
	TotalCents int64  `json:"total_cents,omitzero"`
	Reason     string `json:"reason,omitzero"`
}

// Activities retry on transient errors only; business errors are marked
// non-retryable in activities.go.
var activityOptions = workflow.ActivityOptions{
	StartToCloseTimeout: 10 * time.Second,
	RetryPolicy: &temporal.RetryPolicy{
		InitialInterval:    time.Second,
		BackoffCoefficient: 2,
		MaximumInterval:    30 * time.Second,
		MaximumAttempts:    5,
	},
}

// CreateHold gets one attempt. It is not idempotent: if it succeeded but the
// result was lost, a retry would find its own seats taken. A hold orphaned
// that way simply expires.
var createHoldOptions = workflow.ActivityOptions{
	StartToCloseTimeout: 10 * time.Second,
	RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
}

func CheckoutWorkflow(ctx workflow.Context, in Input) (Result, error) {
	ctx = workflow.WithActivityOptions(ctx, activityOptions)
	var a *Activities // only used to name activity methods

	var hold HoldInfo
	holdCtx := workflow.WithActivityOptions(ctx, createHoldOptions)
	if err := workflow.ExecuteActivity(holdCtx, a.CreateHold, in).Get(ctx, &hold); err != nil {
		return Result{}, err
	}

	// Wait for the pay signal or the hold's expiry, whichever comes first.
	// The timer is durable: it fires even if every worker restarts meanwhile.
	timerCtx, cancelTimer := workflow.WithCancel(ctx)
	defer cancelTimer()
	expired := workflow.NewTimer(timerCtx, hold.ExpiresAt.Sub(workflow.Now(ctx)))
	var pay PayRequest
	paid := false
	sel := workflow.NewSelector(ctx)
	sel.AddReceive(workflow.GetSignalChannel(ctx, PaySignal), func(c workflow.ReceiveChannel, _ bool) {
		c.Receive(ctx, &pay)
		paid = true
	})
	sel.AddFuture(expired, func(workflow.Future) {})
	sel.Select(ctx)

	if !paid {
		// The seats are already available again by the expiry rule; releasing
		// just tidies the rows.
		if err := release(ctx, hold, in.BuyerID); err != nil {
			return Result{}, err
		}
		return Result{Status: StatusHoldExpired, HoldID: hold.HoldID}, nil
	}

	var chargeID string
	charge := ChargeInput{
		IdempotencyKey: in.IdempotencyKey + ":charge",
		BuyerID:        in.BuyerID,
		AmountCents:    hold.TotalCents,
		PaymentToken:   pay.PaymentToken,
	}
	if err := workflow.ExecuteActivity(ctx, a.ChargePayment, charge).Get(ctx, &chargeID); err != nil {
		if relErr := release(ctx, hold, in.BuyerID); relErr != nil {
			return Result{}, relErr
		}
		return Result{Status: StatusPaymentFailed, HoldID: hold.HoldID, Reason: err.Error()}, nil
	}

	var order store.Order
	purchase := PurchaseInput{HoldID: hold.HoldID, BuyerID: in.BuyerID, IdempotencyKey: in.IdempotencyKey}
	if err := workflow.ExecuteActivity(ctx, a.Purchase, purchase).Get(ctx, &order); err != nil {
		// The buyer paid but got no seats (e.g. the hold expired during the
		// charge). Undo in reverse order: refund, then release.
		if refErr := workflow.ExecuteActivity(ctx, a.RefundPayment, chargeID).Get(ctx, nil); refErr != nil {
			return Result{}, refErr
		}
		if relErr := release(ctx, hold, in.BuyerID); relErr != nil {
			return Result{}, relErr
		}
		return Result{Status: StatusPurchaseFailed, HoldID: hold.HoldID, ChargeID: chargeID, Reason: err.Error()}, nil
	}

	return Result{
		Status:     StatusPurchased,
		HoldID:     hold.HoldID,
		OrderID:    order.ID,
		ChargeID:   chargeID,
		TotalCents: order.TotalCents,
	}, nil
}

func release(ctx workflow.Context, hold HoldInfo, buyerID string) error {
	var a *Activities
	return workflow.ExecuteActivity(ctx, a.ReleaseHold, ReleaseInput{HoldID: hold.HoldID, BuyerID: buyerID}).Get(ctx, nil)
}
