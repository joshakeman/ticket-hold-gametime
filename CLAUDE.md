# CLAUDE.md

Go 1.26 seat-hold/purchase service on Postgres 16 (pgx v5, stdlib `net/http`
router). See README.md for the API and the concurrency design.

## Commands

- `make test` — starts the DB and runs `go test -race -count=1 ./...` with
  `TEST_DATABASE_URL` set. **Plain `go test ./...` skips every DB test and still
  prints `ok`**; never report tests as passing from that.
- `make run` / `make seed` — serve on :8080 / add a "Demo Game" event with 20 seats.
- DB: `docker compose` service `db`, host port **5433**, user/pass/db `tickets`.
  psql: `docker compose exec db psql -U tickets -d tickets`.

## Architecture rules

- All SQL lives in `internal/store`. `internal/api` only parses input, calls the
  store and maps errors to status codes in `writeStoreError`.
- Seat state changes are single conditional (compare-and-set) UPDATEs with
  `RETURNING`; the row count decides success. Don't replace this with
  read-then-write logic or Go-side locks.
- Lock seat rows in ascending id order (`sortedUnique` + `ORDER BY id FOR
  UPDATE`) to avoid deadlocks. Purchase/release serialize on the hold row via
  `lockHold`.
- Use `s.inTx` for multi-statement work. Its closure can run twice (retry on
  40P01/40001), so reset any captured outputs at the top of the closure.
- Hold expiry is lazy: availability is always
  `order_id IS NULL AND (hold_id IS NULL OR hold_expires_at <= now())`. Any new
  query touching seat availability must apply the same predicate. No sweeper.
- Use `now()` in SQL for time comparisons, not Go's `time.Now()`, so the DB
  clock is the single source of truth.
- New store errors: add a sentinel in `store.go` and a case in
  `api.writeStoreError`, or it becomes a 500.

## Migrations

Add a new numbered file `migrations/000N_name.sql`; never edit an applied one.
Files are embedded and applied in filename order at startup, under an advisory
lock, each in its own transaction.

## Tests

- Store tests share one DB. Each test calls `newEvent(t, n)` for its own event,
  and idempotency keys go through `key(ev, name)` so they don't collide across
  tests or runs.
- Concurrency tests use a `start` channel so all goroutines fire together;
  keep that pattern and run with `-race`.
- Use `expireHold(t, id)` to simulate expiry instead of sleeping.
