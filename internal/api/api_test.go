package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/josh/ticket-hold-gametime/internal/store"
	"github.com/josh/ticket-hold-gametime/migrations"
)

func newServer(t *testing.T) (srv *httptest.Server, eventID int64, seatIDs []int64) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set (run `make test`)")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO events (name, starts_at) VALUES ($1, now()) RETURNING id`, t.Name()).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO seats (event_id, label, price_cents) VALUES ($1, $2, 2500) RETURNING id`,
			eventID, fmt.Sprintf("S%d", i)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		seatIDs = append(seatIDs, id)
	}
	srv = httptest.NewServer(New(store.New(pool)))
	t.Cleanup(srv.Close)
	return srv, eventID, seatIDs
}

func do(t *testing.T, method, url, buyer, key, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if buyer != "" {
		req.Header.Set("X-Buyer-ID", buyer)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out) //nolint:errcheck // 204 has no body
	return resp.StatusCode, out
}

func TestHoldAndPurchaseOverHTTP(t *testing.T) {
	srv, ev, seats := newServer(t)
	holdsURL := fmt.Sprintf("%s/events/%d/holds", srv.URL, ev)
	body := fmt.Sprintf(`{"seat_ids":[%d,%d]}`, seats[0], seats[1])

	if code, _ := do(t, "POST", holdsURL, "", "", body); code != http.StatusUnauthorized {
		t.Fatalf("no buyer header: %d", code)
	}
	if code, _ := do(t, "POST", holdsURL, "alice", "", `{"seat_ids":[]}`); code != http.StatusBadRequest {
		t.Fatalf("empty seats: %d", code)
	}
	if code, _ := do(t, "POST", holdsURL, "alice", "", `{"seat_ids":[1,2,3,4,5,6,7,8,9]}`); code != http.StatusBadRequest {
		t.Fatalf("too many seats: %d", code)
	}

	code, hold := do(t, "POST", holdsURL, "alice", "", body)
	if code != http.StatusCreated {
		t.Fatalf("hold: %d %v", code, hold)
	}
	holdID := hold["hold_id"].(string)

	code, conflict := do(t, "POST", holdsURL, "bob", "", fmt.Sprintf(`{"seat_ids":[%d]}`, seats[1]))
	if code != http.StatusConflict || len(conflict["unavailable"].([]any)) != 1 {
		t.Fatalf("second buyer: %d %v", code, conflict)
	}

	purchaseURL := fmt.Sprintf("%s/holds/%s/purchase", srv.URL, holdID)
	k := fmt.Sprintf("k-%d", ev)
	if code, _ := do(t, "POST", purchaseURL, "alice", "", ""); code != http.StatusBadRequest {
		t.Fatalf("missing idempotency key: %d", code)
	}
	if code, _ := do(t, "POST", purchaseURL, "bob", k, ""); code != http.StatusForbidden {
		t.Fatalf("wrong buyer: %d", code)
	}
	code, order := do(t, "POST", purchaseURL, "alice", k, "")
	if code != http.StatusCreated || order["total_cents"].(float64) != 5000 {
		t.Fatalf("purchase: %d %v", code, order)
	}
	code, replay := do(t, "POST", purchaseURL, "alice", k, "")
	if code != http.StatusOK || replay["order_id"] != order["order_id"] {
		t.Fatalf("replay: %d %v", code, replay)
	}

	code, list := do(t, "GET", fmt.Sprintf("%s/events/%d/seats", srv.URL, ev), "", "", "")
	if code != http.StatusOK || len(list["seats"].([]any)) != 3 {
		t.Fatalf("list: %d %v", code, list)
	}

	if code, _ := do(t, "GET", srv.URL+"/holds/not-a-uuid", "alice", "", ""); code != http.StatusBadRequest {
		t.Fatalf("bad uuid: %d", code)
	}
	if code, _ := do(t, "GET", srv.URL+"/holds/00000000-0000-0000-0000-000000000000", "alice", "", ""); code != http.StatusNotFound {
		t.Fatalf("missing hold: %d", code)
	}
}

func TestReleaseOverHTTP(t *testing.T) {
	srv, ev, seats := newServer(t)
	holdsURL := fmt.Sprintf("%s/events/%d/holds", srv.URL, ev)
	body := fmt.Sprintf(`{"seat_ids":[%d]}`, seats[0])

	_, hold := do(t, "POST", holdsURL, "alice", "", body)
	holdURL := srv.URL + "/holds/" + hold["hold_id"].(string)

	if code, got := do(t, "GET", holdURL, "alice", "", ""); code != http.StatusOK || got["status"] != "active" {
		t.Fatalf("get hold: %d %v", code, got)
	}
	if code, _ := do(t, "DELETE", holdURL, "alice", "", ""); code != http.StatusNoContent {
		t.Fatalf("release: %d", code)
	}
	if code, _ := do(t, "POST", holdsURL, "bob", "", body); code != http.StatusCreated {
		t.Fatalf("bob after release: %d", code)
	}
}
