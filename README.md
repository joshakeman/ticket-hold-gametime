# ticket-hold-gametime

A small Go service for holding and buying event seats, built so that two
buyers racing for the same seat can never both get it. Postgres decides every
race; there are no in-process locks, no background workers and no Redis.

- Buyers put a **hold** on 1–8 seats. A hold lasts 10 minutes and is all or
  nothing: either every requested seat is held or none are.
- The holder can **purchase** the hold (with an `Idempotency-Key`) or
  **release** it early.
- Expired holds need no cleanup job. A seat is available whenever
  `order_id IS NULL AND (hold_id IS NULL OR hold_expires_at <= now())`, and
  every query applies that rule.

## Running it

Requires Go 1.26+ and Docker.

```bash
make db      # start Postgres 16 on localhost:5433
make run     # apply migrations and serve on :8080
make seed    # (in another terminal) add a "Demo Game" event with 20 seats
make test    # run all tests with -race against the database
```

The server reads `DATABASE_URL` (default
`postgres://tickets:tickets@localhost:5433/tickets?sslmode=disable`) and
`ADDR` (default `:8080`). Migrations in `migrations/` are embedded in the
binary and applied at startup.

Without `TEST_DATABASE_URL`, `go test ./...` skips every database test and
still prints `ok`. Use `make test`.

## API

All buyer-scoped calls need an `X-Buyer-ID` header (a stand-in for real auth).

| Method & path | Body / headers | Success |
|---|---|---|
| `GET /events/{id}/seats` | — | 200 `{"seats":[{id,label,price_cents,status}]}` |
| `POST /events/{id}/holds` | `{"seat_ids":[…]}` (1–8) | 201 hold |
| `GET /holds/{id}` | — | 200 hold with `status`: `active` / `expired` / `purchased` |
| `DELETE /holds/{id}` | — | 204 (releasing twice is fine) |
| `POST /holds/{id}/purchase` | `Idempotency-Key` header | 201 order, or 200 when replayed |

Errors are `{"error": "..."}`:

| Status | When |
|---|---|
| 400 | Bad JSON, bad id, 0 or >8 seats, missing key, seats not in the event (`unknown` lists them) |
| 401 | Missing `X-Buyer-ID` |
| 403 | The hold belongs to another buyer |
| 404 | Event or hold not found |
| 409 | Seats taken (`unavailable` lists them), hold already bought with another key, key reused for another hold |
| 410 | Hold expired or released |

### Example

```bash
EVENT=$(docker compose exec -T db psql -U tickets -d tickets -tA \
  -c "SELECT id FROM events WHERE name = 'Demo Game' ORDER BY id DESC LIMIT 1")

curl -s localhost:8080/events/$EVENT/seats

curl -s -X POST localhost:8080/events/$EVENT/holds \
  -H 'X-Buyer-ID: alice' -d '{"seat_ids":[<seat id>]}'

curl -s -X POST localhost:8080/holds/<hold_id>/purchase \
  -H 'X-Buyer-ID: alice' -H 'Idempotency-Key: order-1'
```

Sending the same purchase again returns the same order with status 200.

## How the concurrency works

**Every seat is its own row, and state changes are compare-and-set UPDATEs.**
A hold claims seats with

```sql
UPDATE seats SET hold_id = $1, hold_expires_at = $2
WHERE event_id = $3 AND id = ANY($4)
  AND order_id IS NULL
  AND (hold_id IS NULL OR hold_expires_at <= now())
RETURNING id
```

If fewer rows come back than were asked for, someone else has a seat, and the
transaction rolls back, so partial holds can't leak.

**Deadlocks are avoided by lock ordering.** Before that UPDATE, `CreateHold`
runs `SELECT … ORDER BY id FOR UPDATE` on the requested seats (sorted and
de-duplicated). Two overlapping multi-seat holds always lock in the same
order. As a backstop, `inTx` retries once on a deadlock (`40P01`) or
serialization failure (`40001`).

**Purchase and release lock the hold row** (`SELECT … FOR UPDATE` on `holds`),
which serializes concurrent calls on the same hold. Purchase then claims seats
only where `hold_id` still matches and `hold_expires_at > now()`, so a lapsed
hold whose seat was taken by someone else comes up short and fails.

**Idempotency.** `orders.hold_id` is UNIQUE (one order per hold), and
`(buyer_id, idempotency_key)` is UNIQUE (one hold per key). A retry with the
same key finds the existing order and returns it. A different key on a bought
hold returns 409.

`seats.order_id` is a `DEFERRABLE INITIALLY DEFERRED` foreign key, so purchase
can mark seats sold before inserting the order row, all in one transaction.

## Layout

```
cmd/server/          main: config, pool, migrations, HTTP server, graceful shutdown
internal/api/        routing, validation, store-error → HTTP status mapping
internal/store/      all SQL: hold.go, purchase.go, seats.go, migrate.go, store.go (tx helper, errors)
migrations/          embedded *.sql schema files
scripts/seed.sql     demo data
```

## Tests

`internal/store/store_test.go` is the important one:

- `TestHoldRaceSingleSeat`: 50 goroutines race for one seat, and exactly one wins.
- `TestOverlappingMultiSeatHoldsAreAllOrNothing`: 100 buyers each ask for 3 of
  6 seats in random order. The test checks there are no deadlocks, no
  double-held seats and no partial holds.
- `TestConcurrentPurchaseSameHoldIsIdempotent`: 20 concurrent purchases with
  one key produce exactly one order.
- There are also tests for expiry, takeover of expired holds, release,
  validation and seat status.

`internal/api/api_test.go` runs the HTTP flow end to end against the database.

## Claude Code skills

`CLAUDE.md` has project notes for Claude Code, and `.claude/skills/` has:

- `/test`: runs the suite against the real database and flags skipped tests.
- `/demo`: starts the server and walks through hold → conflict → purchase → replay with curl.
- `/check-comments`: read-only review of the current diff (or named files) for
  comments that contradict the code, overclaim, or restate the obvious.
  Adapted from [Ardan Labs' service repo](https://github.com/ardanlabs/service)
  (Apache 2.0).
- `use-modern-go`: loads automatically when writing Go. It detects the Go
  version from `go.mod` (1.26) and applies modern idioms up to that version.
  Adapted from Ardan Labs' service repo (Apache 2.0).
- `money-safety`: loads automatically when you edit store or migration code.
  It holds the rules that stop seats from being sold twice or buyers from
  being charged twice.
