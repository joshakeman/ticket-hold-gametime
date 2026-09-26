---
name: money-safety
description: Rules for code that holds, sells, or charges for seats. Use when editing hold, purchase, release, or payment code, or writing SQL that changes seat or order state.
user-invocable: false
paths: "internal/store/**, migrations/**"
---
These rules exist because selling a seat twice or charging a buyer twice is the worst bug this service can have.

- Every change to seat or order state happens inside one transaction via `inTx`. The closure may run twice, so reset any captured output at the top.
- Claim seats with a compare-and-set `UPDATE ... WHERE <still available> RETURNING id` and compare the returned count to what was requested. Never SELECT availability and then UPDATE in a separate step.
- When locking several seats, lock them in ascending id order (`sortedUnique` + `ORDER BY id FOR UPDATE`) so overlapping requests cannot deadlock.
- Serialize operations on one hold by locking the hold row (`lockHold`) first.
- Purchases require an Idempotency-Key. The same key on the same hold returns the stored order unchanged; a different key or a reused key on another hold is a conflict.
- Prices come from the `seats` table, never from the request body.
- Time comes from Postgres `now()`, never the Go clock.
- Never make network calls (payments, notifications) inside a transaction that holds seat locks. Commit a pending state first, call out, then update.
- Any change here must keep the concurrency tests passing under `make test` (race detector on, real Postgres). A run where tests skip is not a pass.
