---
name: demo
description: Start the server and walk through list seats → hold → conflict → purchase → idempotent replay with curl. Use when asked to run, demo or smoke-test the service.
---

1. Start the DB (`make db`) and run the server in the background:
   `DATABASE_URL='postgres://tickets:tickets@localhost:5433/tickets?sslmode=disable' ADDR=:8080 go run ./cmd/server`.
   Wait for "listening on :8080".
2. Find the demo event, and run `make seed` first if there isn't one:

   ```bash
   docker compose exec -T db psql -U tickets -d tickets -tA -c "SELECT id FROM events WHERE name = 'Demo Game' ORDER BY id DESC LIMIT 1"
   ```
3. Run through this sequence and show each response with its status code
   (`curl -s -w ' %{http_code}\n'`):
   - `GET /events/$EVENT/seats`, then pick two `available` seat ids.
   - `POST /events/$EVENT/holds` as `X-Buyer-ID: alice` with both seats → 201.
   - The same seats as `bob` → 409 with `unavailable`.
   - `POST /holds/$HOLD/purchase` as alice with `Idempotency-Key: demo-<timestamp>` → 201.
   - The same purchase again → 200 with the same `order_id`.
   - `GET /holds/$HOLD` as alice → `status: purchased`.
4. Stop the server when you're done. Note that the demo permanently sells
   those seats in the dev DB, so run `make seed` again for fresh ones.
