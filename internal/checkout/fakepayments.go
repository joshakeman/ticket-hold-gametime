package checkout

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// DeclineToken makes FakePayments decline the charge.
const DeclineToken = "tok_decline"

// FakePayments is an in-memory Payments for tests and local runs. Like a
// real provider, it dedupes charges by idempotency key.
type FakePayments struct {
	mu       sync.Mutex
	charges  map[string]string // idempotency key -> charge id
	refunded map[string]bool
	calls    int

	// LoseNextResponse makes the next Charge record the charge but return an
	// error, as if the response were lost on the network.
	LoseNextResponse bool
}

func NewFakePayments() *FakePayments {
	return &FakePayments{charges: map[string]string{}, refunded: map[string]bool{}}
}

func (f *FakePayments) Charge(_ context.Context, key, _ string, _ int64, paymentToken string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if paymentToken == DeclineToken {
		return "", ErrCardDeclined
	}
	id, ok := f.charges[key]
	if !ok {
		id = fmt.Sprintf("ch_%d", len(f.charges)+1)
		f.charges[key] = id
	}
	if f.LoseNextResponse {
		f.LoseNextResponse = false
		return "", errors.New("connection reset")
	}
	return id, nil
}

func (f *FakePayments) Refund(_ context.Context, chargeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refunded[chargeID] = true
	return nil
}

// Stats reports how many Charge calls were made and how many distinct
// charges exist.
func (f *FakePayments) Stats() (calls, charges int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, len(f.charges)
}
