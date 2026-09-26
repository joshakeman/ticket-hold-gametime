package checkout

import (
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	"github.com/josh/ticket-hold-gametime/internal/store"
)

// These tests run the workflow in Temporal's in-process test environment:
// no server, and time is simulated, so a 10-minute timer fires instantly.

var a *Activities // names activity methods for mocks and assertions

var input = Input{EventID: 1, BuyerID: "alice", SeatIDs: []int64{10, 11}, IdempotencyKey: "k1"}

func newEnv(t *testing.T, acts *Activities) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(CheckoutWorkflow)
	env.RegisterActivity(acts)
	return env
}

// holdFor mocks CreateHold with a hold that Postgres says has 10 minutes left.
func holdFor(env *testsuite.TestWorkflowEnvironment) HoldInfo {
	h := HoldInfo{
		HoldID: "h1", SeatIDs: input.SeatIDs, TotalCents: 5000,
		ExpiresAt: env.Now().Add(10 * time.Minute),
		ExpiresIn: 10 * time.Minute,
	}
	env.OnActivity(a.CreateHold, mock.Anything, input).Return(h, nil).Once()
	return h
}

func pay(env *testsuite.TestWorkflowEnvironment, token string) {
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(PaySignal, PayRequest{PaymentToken: token})
	}, time.Minute)
}

func result(t *testing.T, env *testsuite.TestWorkflowEnvironment) Result {
	t.Helper()
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var r Result
	if err := env.GetWorkflowResult(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCheckoutHappyPath(t *testing.T) {
	env := newEnv(t, &Activities{})
	holdFor(env)
	pay(env, "tok_ok")
	env.OnActivity(a.ChargePayment, mock.Anything, ChargeInput{
		IdempotencyKey: "k1:charge", BuyerID: "alice", AmountCents: 5000, PaymentToken: "tok_ok",
	}).Return("ch_1", nil).Once()
	env.OnActivity(a.Purchase, mock.Anything, PurchaseInput{HoldID: "h1", BuyerID: "alice", IdempotencyKey: "k1"}).
		Return(store.Order{ID: "o1", HoldID: "h1", TotalCents: 5000}, nil).Once()

	env.ExecuteWorkflow(CheckoutWorkflow, input)

	r := result(t, env)
	if r.Status != StatusPurchased || r.OrderID != "o1" || r.ChargeID != "ch_1" {
		t.Fatalf("result = %+v", r)
	}
	env.AssertExpectations(t)
}

func TestCheckoutHoldExpiresWithoutPayment(t *testing.T) {
	env := newEnv(t, &Activities{})
	holdFor(env)
	env.OnActivity(a.ReleaseHold, mock.Anything, ReleaseInput{HoldID: "h1", BuyerID: "alice"}).Return(nil).Once()
	env.OnActivity(a.ChargePayment, mock.Anything, mock.Anything).Return("", nil).Maybe()

	env.ExecuteWorkflow(CheckoutWorkflow, input) // no pay signal

	if r := result(t, env); r.Status != StatusHoldExpired {
		t.Fatalf("result = %+v", r)
	}
	env.AssertExpectations(t)
	env.AssertActivityNotCalled(t, "ChargePayment", mock.Anything, mock.Anything)
}

// The timer must use the duration Postgres computed, not ExpiresAt compared
// with Temporal's clock. Here Postgres's clock is an hour behind Temporal's,
// so ExpiresAt looks long past, yet the hold has 10 minutes left and a
// payment at minute 1 must succeed.
func TestCheckoutTimerIgnoresClockSkew(t *testing.T) {
	env := newEnv(t, &Activities{})
	skewed := HoldInfo{
		HoldID: "h1", SeatIDs: input.SeatIDs, TotalCents: 5000,
		ExpiresAt: env.Now().Add(-50 * time.Minute),
		ExpiresIn: 10 * time.Minute,
	}
	env.OnActivity(a.CreateHold, mock.Anything, input).Return(skewed, nil).Once()
	pay(env, "tok_ok")
	env.OnActivity(a.ChargePayment, mock.Anything, mock.Anything).Return("ch_1", nil).Once()
	env.OnActivity(a.Purchase, mock.Anything, mock.Anything).Return(store.Order{ID: "o1"}, nil).Once()
	env.OnActivity(a.ReleaseHold, mock.Anything, mock.Anything).Return(nil).Maybe()

	env.ExecuteWorkflow(CheckoutWorkflow, input)

	if r := result(t, env); r.Status != StatusPurchased {
		t.Fatalf("result = %+v, want purchased (timer fired early?)", r)
	}
	env.AssertActivityNotCalled(t, "ReleaseHold", mock.Anything, mock.Anything)
}

// Expiry must wait the full duration: a payment at minute 9 still succeeds,
// while one at minute 11 finds the hold released.
func TestCheckoutTimerFiresAfterExpiresIn(t *testing.T) {
	for _, tc := range []struct {
		payAt time.Duration
		want  string
	}{
		{9 * time.Minute, StatusPurchased},
		{11 * time.Minute, StatusHoldExpired},
	} {
		t.Run(tc.payAt.String(), func(t *testing.T) {
			env := newEnv(t, &Activities{})
			holdFor(env)
			env.RegisterDelayedCallback(func() {
				env.SignalWorkflow(PaySignal, PayRequest{PaymentToken: "tok_ok"})
			}, tc.payAt)
			env.OnActivity(a.ChargePayment, mock.Anything, mock.Anything).Return("ch_1", nil).Maybe()
			env.OnActivity(a.Purchase, mock.Anything, mock.Anything).Return(store.Order{ID: "o1"}, nil).Maybe()
			env.OnActivity(a.ReleaseHold, mock.Anything, mock.Anything).Return(nil).Maybe()

			env.ExecuteWorkflow(CheckoutWorkflow, input)

			if r := result(t, env); r.Status != tc.want {
				t.Fatalf("result = %+v, want %s", r, tc.want)
			}
		})
	}
}

func TestCheckoutChargeDeclinedReleasesHold(t *testing.T) {
	env := newEnv(t, &Activities{})
	holdFor(env)
	pay(env, DeclineToken)
	env.OnActivity(a.ChargePayment, mock.Anything, mock.Anything).Return("", businessError(ErrCardDeclined)).Once()
	env.OnActivity(a.ReleaseHold, mock.Anything, ReleaseInput{HoldID: "h1", BuyerID: "alice"}).Return(nil).Once()
	env.OnActivity(a.Purchase, mock.Anything, mock.Anything).Return(store.Order{}, nil).Maybe()

	env.ExecuteWorkflow(CheckoutWorkflow, input)

	if r := result(t, env); r.Status != StatusPaymentFailed {
		t.Fatalf("result = %+v", r)
	}
	env.AssertExpectations(t) // Once() also fails the test if a declined card were retried
	env.AssertActivityNotCalled(t, "Purchase", mock.Anything, mock.Anything)
}

func TestCheckoutPurchaseFailureRefundsThenReleases(t *testing.T) {
	env := newEnv(t, &Activities{})
	holdFor(env)
	pay(env, "tok_ok")
	env.OnActivity(a.ChargePayment, mock.Anything, mock.Anything).Return("ch_1", nil).Once()
	env.OnActivity(a.Purchase, mock.Anything, mock.Anything).Return(store.Order{}, businessError(store.ErrHoldExpired)).Once()

	var order []string
	env.OnActivity(a.RefundPayment, mock.Anything, "ch_1").Return(nil).Once().
		Run(func(mock.Arguments) { order = append(order, "refund") })
	env.OnActivity(a.ReleaseHold, mock.Anything, mock.Anything).Return(nil).Once().
		Run(func(mock.Arguments) { order = append(order, "release") })

	env.ExecuteWorkflow(CheckoutWorkflow, input)

	if r := result(t, env); r.Status != StatusPurchaseFailed || r.ChargeID != "ch_1" {
		t.Fatalf("result = %+v", r)
	}
	env.AssertExpectations(t)
	if len(order) != 2 || order[0] != "refund" || order[1] != "release" {
		t.Fatalf("compensation order = %v, want [refund release]", order)
	}
}

// A charge whose response is lost gets retried with the same idempotency key,
// and the provider must not charge twice. This uses the real ChargePayment
// activity with FakePayments.
func TestCheckoutRetriedChargeChargesOnce(t *testing.T) {
	payments := NewFakePayments()
	payments.LoseNextResponse = true
	env := newEnv(t, &Activities{Payments: payments})
	holdFor(env)
	pay(env, "tok_ok")
	env.OnActivity(a.Purchase, mock.Anything, mock.Anything).
		Return(store.Order{ID: "o1", HoldID: "h1", TotalCents: 5000}, nil).Once()

	env.ExecuteWorkflow(CheckoutWorkflow, input)

	if r := result(t, env); r.Status != StatusPurchased || r.ChargeID != "ch_1" {
		t.Fatalf("result = %+v", r)
	}
	calls, charges := payments.Stats()
	if calls != 2 || charges != 1 {
		t.Fatalf("Charge called %d times creating %d charges, want 2 calls and 1 charge", calls, charges)
	}
}
