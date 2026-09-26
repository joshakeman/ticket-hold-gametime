package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/josh/ticket-hold-gametime/migrations"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		fmt.Println("TEST_DATABASE_URL not set; skipping store tests (run `make test`)")
		os.Exit(0)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	if err := Migrate(ctx, pool, migrations.FS); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	testPool = pool
	code := m.Run()
	pool.Close()
	os.Exit(code)
}

// newEvent creates an event with n seats at 1000 cents each. Every test uses
// its own event, so tests never interfere with each other.
func newEvent(t *testing.T, n int) (s *Store, eventID int64, seatIDs []int64) {
	t.Helper()
	ctx := context.Background()
	if err := testPool.QueryRow(ctx,
		`INSERT INTO events (name, starts_at) VALUES ($1, now()) RETURNING id`, t.Name()).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		var id int64
		if err := testPool.QueryRow(ctx,
			`INSERT INTO seats (event_id, label, price_cents) VALUES ($1, $2, 1000) RETURNING id`,
			eventID, fmt.Sprintf("S%d", i)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		seatIDs = append(seatIDs, id)
	}
	return New(testPool), eventID, seatIDs
}

// expireHold back-dates a hold and its seats, as if 10 minutes had passed.
func expireHold(t *testing.T, holdID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`UPDATE seats SET hold_expires_at = now() - interval '1 second' WHERE hold_id = $1`, holdID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE holds SET expires_at = now() - interval '1 second' WHERE id = $1`, holdID); err != nil {
		t.Fatal(err)
	}
}

func TestHoldRaceSingleSeat(t *testing.T) {
	s, ev, seats := newEvent(t, 1)
	const buyers = 50

	var wins, unavailable atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < buyers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.CreateHold(context.Background(), ev, fmt.Sprintf("buyer-%d", i), seats)
			var ue *UnavailableError
			switch {
			case err == nil:
				wins.Add(1)
			case errors.As(err, &ue):
				unavailable.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if wins.Load() != 1 || unavailable.Load() != buyers-1 {
		t.Fatalf("wins=%d unavailable=%d, want 1 and %d", wins.Load(), unavailable.Load(), buyers-1)
	}
}

func TestOverlappingMultiSeatHoldsAreAllOrNothing(t *testing.T) {
	s, ev, seats := newEvent(t, 6)
	const buyers = 100

	type result struct {
		holdID string
		seats  []int64
	}
	var mu sync.Mutex
	var won []result

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < buyers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(i)))
			want := make([]int64, 0, 3)
			for _, idx := range rng.Perm(len(seats))[:3] { // random order, so lock order matters
				want = append(want, seats[idx])
			}
			<-start
			h, err := s.CreateHold(context.Background(), ev, fmt.Sprintf("buyer-%d", i), want)
			var ue *UnavailableError
			switch {
			case err == nil:
				mu.Lock()
				won = append(won, result{h.ID, h.SeatIDs})
				mu.Unlock()
			case errors.As(err, &ue):
			default:
				t.Errorf("unexpected error (deadlock?): %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	// Every winner holds all 3 of its seats, and no seat is held twice.
	owner := map[int64]string{}
	for _, w := range won {
		if len(w.seats) != 3 {
			t.Fatalf("hold %s has %d seats", w.holdID, len(w.seats))
		}
		for _, id := range w.seats {
			if prev, dup := owner[id]; dup {
				t.Fatalf("seat %d held by both %s and %s", id, prev, w.holdID)
			}
			owner[id] = w.holdID
		}
	}
	var held int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM seats WHERE event_id = $1 AND hold_id IS NOT NULL`, ev).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != len(owner) {
		t.Fatalf("db shows %d held seats, winners account for %d (partial hold leaked)", held, len(owner))
	}
	if len(won) == 0 {
		t.Fatal("expected at least one winner")
	}
}

func TestHoldValidation(t *testing.T) {
	s, ev, seats := newEvent(t, 2)
	ctx := context.Background()

	if _, err := s.CreateHold(ctx, ev+1_000_000, "a", seats); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("missing event: got %v", err)
	}

	var ue *UnknownSeatsError
	_, err := s.CreateHold(ctx, ev, "a", []int64{seats[0], seats[1] + 1_000_000})
	if !errors.As(err, &ue) || len(ue.SeatIDs) != 1 {
		t.Fatalf("unknown seat: got %v", err)
	}
	// The failed request must not have taken the valid seat.
	if _, err := s.CreateHold(ctx, ev, "b", []int64{seats[0]}); err != nil {
		t.Fatalf("seat leaked by failed hold: %v", err)
	}

	// Duplicate ids in one request are collapsed.
	h, err := s.CreateHold(ctx, ev, "c", []int64{seats[1], seats[1]})
	if err != nil || len(h.SeatIDs) != 1 {
		t.Fatalf("dup ids: %v %v", h, err)
	}
}

func TestPurchaseFlow(t *testing.T) {
	s, ev, seats := newEvent(t, 3)
	ctx := context.Background()

	h, err := s.CreateHold(ctx, ev, "alice", seats[:2])
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Purchase(ctx, h.ID, "mallory", key(ev, "k1")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other buyer purchase: got %v", err)
	}

	o, replayed, err := s.Purchase(ctx, h.ID, "alice", key(ev, "k1"))
	if err != nil || replayed {
		t.Fatalf("purchase: %v replayed=%v", err, replayed)
	}
	if o.TotalCents != 2000 || len(o.SeatIDs) != 2 {
		t.Fatalf("order = %+v", o)
	}

	// Same key: same order back.
	o2, replayed, err := s.Purchase(ctx, h.ID, "alice", key(ev, "k1"))
	if err != nil || !replayed || o2.ID != o.ID {
		t.Fatalf("replay: %+v replayed=%v err=%v", o2, replayed, err)
	}
	// Different key on an already-bought hold.
	if _, _, err := s.Purchase(ctx, h.ID, "alice", key(ev, "k2")); !errors.Is(err, ErrAlreadyPurchased) {
		t.Fatalf("second key: got %v", err)
	}
	// Same key reused for a different hold.
	h2, err := s.CreateHold(ctx, ev, "alice", seats[2:])
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Purchase(ctx, h2.ID, "alice", key(ev, "k1")); !errors.Is(err, ErrIdempotencyKeyReuse) {
		t.Fatalf("key reuse: got %v", err)
	}

	// Sold seats can't be held, and releasing a purchased hold is refused.
	var ue *UnavailableError
	if _, err := s.CreateHold(ctx, ev, "bob", seats[:1]); !errors.As(err, &ue) {
		t.Fatalf("hold sold seat: got %v", err)
	}
	if err := s.Release(ctx, h.ID, "alice"); !errors.Is(err, ErrAlreadyPurchased) {
		t.Fatalf("release purchased: got %v", err)
	}

	got, err := s.GetHold(ctx, h.ID, "alice")
	if err != nil || got.Status != "purchased" || len(got.SeatIDs) != 2 {
		t.Fatalf("GetHold: %+v %v", got, err)
	}
}

func TestConcurrentPurchaseSameHoldIsIdempotent(t *testing.T) {
	s, ev, seats := newEvent(t, 2)
	ctx := context.Background()
	h, err := s.CreateHold(ctx, ev, "alice", seats)
	if err != nil {
		t.Fatal(err)
	}

	const n = 20
	ids := make(chan string, n)
	var firsts atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, replayed, err := s.Purchase(ctx, h.ID, "alice", key(ev, "same-key"))
			if err != nil {
				t.Errorf("purchase: %v", err)
				return
			}
			if !replayed {
				firsts.Add(1)
			}
			ids <- o.ID
		}()
	}
	wg.Wait()
	close(ids)

	var first string
	for id := range ids {
		if first == "" {
			first = id
		} else if id != first {
			t.Fatalf("got two different orders: %s and %s", first, id)
		}
	}
	if firsts.Load() != 1 {
		t.Fatalf("%d purchases created an order, want 1", firsts.Load())
	}
	var orders int
	testPool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE hold_id = $1`, h.ID).Scan(&orders) //nolint:errcheck
	if orders != 1 {
		t.Fatalf("%d orders rows", orders)
	}
}

func TestExpiredHoldCanBeTakenAndCannotBePurchased(t *testing.T) {
	s, ev, seats := newEvent(t, 1)
	ctx := context.Background()

	a, err := s.CreateHold(ctx, ev, "alice", seats)
	if err != nil {
		t.Fatal(err)
	}
	var ue *UnavailableError
	if _, err := s.CreateHold(ctx, ev, "bob", seats); !errors.As(err, &ue) {
		t.Fatalf("live hold should block bob: %v", err)
	}

	expireHold(t, a.ID)

	got, err := s.GetHold(ctx, a.ID, "alice")
	if err != nil || got.Status != "expired" {
		t.Fatalf("GetHold after expiry: %+v %v", got, err)
	}
	if _, _, err := s.Purchase(ctx, a.ID, "alice", key(ev, "k")); !errors.Is(err, ErrHoldExpired) {
		t.Fatalf("expired purchase: got %v", err)
	}

	b, err := s.CreateHold(ctx, ev, "bob", seats)
	if err != nil {
		t.Fatalf("bob should get the expired seat: %v", err)
	}
	// Alice's stale hold must not be able to buy the seat bob now holds.
	if _, _, err := s.Purchase(ctx, a.ID, "alice", key(ev, "k")); !errors.Is(err, ErrHoldExpired) {
		t.Fatalf("alice after takeover: got %v", err)
	}
	if _, _, err := s.Purchase(ctx, b.ID, "bob", key(ev, "k")); err != nil {
		t.Fatalf("bob purchase: %v", err)
	}
}

func TestReleaseFreesSeats(t *testing.T) {
	s, ev, seats := newEvent(t, 1)
	ctx := context.Background()

	a, err := s.CreateHold(ctx, ev, "alice", seats)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, a.ID, "bob"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("release by other buyer: %v", err)
	}
	if err := s.Release(ctx, a.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, a.ID, "alice"); err != nil {
		t.Fatalf("second release should be a no-op: %v", err)
	}
	if _, err := s.CreateHold(ctx, ev, "bob", seats); err != nil {
		t.Fatalf("bob after release: %v", err)
	}
	if _, _, err := s.Purchase(ctx, a.ID, "alice", key(ev, "k")); !errors.Is(err, ErrHoldExpired) {
		t.Fatalf("purchase after release: got %v", err)
	}
}

func TestListSeatsStatuses(t *testing.T) {
	s, ev, seats := newEvent(t, 3)
	ctx := context.Background()

	sold, err := s.CreateHold(ctx, ev, "alice", seats[:1])
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Purchase(ctx, sold.ID, "alice", key(ev, "k")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateHold(ctx, ev, "bob", seats[1:2]); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListSeats(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sold", "held", "available"}
	for i, st := range list {
		if st.Status != want[i] {
			t.Errorf("seat %d: %s, want %s", st.ID, st.Status, want[i])
		}
	}
	if _, err := s.ListSeats(ctx, ev+1_000_000); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("missing event: %v", err)
	}
}

// key namespaces an idempotency key by event. Keys are unique per buyer, and
// tests share one database, so a bare "k" would collide across tests and runs.
func key(eventID int64, name string) string { return fmt.Sprintf("%d-%s", eventID, name) }
