# System diagrams

Mermaid diagrams of the service; GitHub renders them inline. Each one ends with a one-line summary.

## System overview

There are two ways in. The HTTP API (`cmd/server`) handles holds and purchases directly. The Temporal worker (`cmd/worker`) runs the same store code as a durable checkout workflow. Everything that touches seat state goes through `internal/store`, and Postgres decides every race.

```mermaid
flowchart LR
  client["HTTP client"] -->|"X-Buyer-ID, Idempotency-Key"| api["internal/api<br/>routing, validation,<br/>error → status mapping"]
  api --> store["internal/store<br/>all SQL, inTx helper"]
  store --> pg[("Postgres 16<br/>events, seats, holds, orders")]
  starter["temporal CLI or app"] -->|"start workflow,<br/>pay signal"| ts["Temporal server<br/>durable history, timers"]
  ts -->|"workflow + activity tasks"| worker["cmd/worker<br/>CheckoutWorkflow + activities"]
  worker --> store
  worker --> payments["Payments interface<br/>FakePayments today"]
```

> All SQL lives in the store package, and every seat change is a single conditional UPDATE, so the database decides who wins. Temporal adds durable orchestration on top: waiting, retries and compensation.

## Data model

Each seat is its own row, so one row is the unit of contention. A seat is available when `order_id IS NULL AND (hold_id IS NULL OR hold_expires_at <= now())`. `orders.hold_id` is unique (one order per hold), and `(buyer_id, idempotency_key)` is unique (one hold per key). `seats.order_id` is a deferred foreign key, so a purchase can mark seats sold before inserting the order, in one transaction.

```mermaid
erDiagram
  events ||--o{ seats : "has"
  events ||--o{ holds : "is held in"
  holds |o--o{ seats : "hold_id"
  holds ||--o| orders : "becomes"
  orders |o--o{ seats : "order_id"
  events {
    bigint id PK
    text name
    timestamptz starts_at
  }
  seats {
    bigint id PK
    bigint event_id FK
    text label
    int price_cents
    uuid hold_id FK
    timestamptz hold_expires_at
    uuid order_id FK "deferred FK"
  }
  holds {
    uuid id PK
    bigint event_id FK
    text buyer_id
    int seat_count
    timestamptz expires_at
  }
  orders {
    uuid id PK
    uuid hold_id FK "UNIQUE: one order per hold"
    text buyer_id
    bigint total_cents
    text idempotency_key "UNIQUE with buyer_id"
  }
```

> The state lives on the seat row, so contention is per seat, not per event.

## Seat lifecycle

There is no background job for expiry. An expired hold simply stops matching the held condition, and the next CreateHold can claim the seat. Release frees seats right away. Purchase only succeeds while the hold is still live.

```mermaid
stateDiagram-v2
  [*] --> Available
  Available --> Held: CreateHold claims it (compare-and-set)
  Held --> Sold: Purchase while hold is live
  Held --> Available: Release (DELETE /holds/id)
  Held --> Available: hold_expires_at passes, checked by every query
  Sold --> [*]
```

> Expiry is checked lazily against the database clock, so there is no sweeper to fall behind or crash.

## Two buyers race for overlapping seats (CreateHold)

Both buyers lock seats in ascending id order with `SELECT … ORDER BY id FOR UPDATE`, so the second waits without holding anything and no deadlock can form. The conditional UPDATE then decides the winner: it sees a fresh snapshot and returns fewer rows than requested if anyone else got there first. Sorting the ids in Go does not set the lock order; only the SQL `ORDER BY` does.

```mermaid
sequenceDiagram
  autonumber
  participant A as Alice (seats 7, 5)
  participant B as Bob (seats 5, 7)
  participant DB as Postgres
  A->>DB: BEGIN, SELECT seats ORDER BY id FOR UPDATE
  DB-->>A: locks seat 5, then seat 7
  B->>DB: BEGIN, SELECT seats ORDER BY id FOR UPDATE
  Note over B,DB: Bob waits on seat 5 while holding nothing, so no deadlock
  A->>DB: INSERT hold, UPDATE seats SET hold_id WHERE still available
  DB-->>A: 2 rows returned, all seats claimed
  A->>DB: COMMIT
  DB-->>B: Bob gets the locks on 5 and 7
  B->>DB: INSERT hold, UPDATE seats SET hold_id WHERE still available
  DB-->>B: 0 rows, fresh snapshot sees Alice's hold
  B->>DB: ROLLBACK
  Note over A,B: Alice gets 201 Created, Bob gets 409 with the unavailable seats
```

> Deadlocks need a cycle. If every transaction locks in the same order, a cycle can’t form.

## A retried purchase gets the same order (idempotency)

Purchase reads before it writes: it checks whether this hold already has an order. Locking the hold row first makes concurrent requests take turns, so a retry sees the committed order and replays it (200) instead of failing with “seats lost”. With that lock removed, the concurrency test fails 9 times out of 10.

```mermaid
sequenceDiagram
  autonumber
  participant R1 as Purchase (key k1)
  participant R2 as Retry (same key k1)
  participant DB as Postgres
  R1->>DB: lockHold: SELECT hold FOR UPDATE
  R2->>DB: lockHold: SELECT hold FOR UPDATE
  Note over R2,DB: R2 blocks until R1 commits
  R1->>DB: any order for this hold? none
  R1->>DB: UPDATE seats SET order_id WHERE hold still live
  R1->>DB: INSERT order with key k1
  R1->>DB: COMMIT
  Note left of R1: 201 Created
  DB-->>R2: hold lock granted
  R2->>DB: any order for this hold? yes, with key k1
  Note left of R2: 200 OK, the same order (replay)
```

> The seat UPDATE keeps it correct. The hold lock keeps retries idempotent.

## Checkout workflow in Temporal (saga)

The workflow waits durably for payment or expiry. The timer runs for a duration Postgres computed, so the two clocks are never compared. Each failure undoes earlier steps in reverse. Transient errors retry with backoff, and business errors (declined card, expired hold) do not. Every retried step is idempotent. CreateHold isn’t, so it gets one attempt.

```mermaid
flowchart TD
  start(["Start CheckoutWorkflow"]) --> hold["CreateHold activity<br/>one attempt: not idempotent"]
  hold -->|"seats taken"| unavailable(["workflow fails: unavailable"])
  hold --> wait{"Wait for pay signal<br/>or timer for ExpiresIn"}
  wait -->|"timer fires first"| rel1["ReleaseHold"] --> expired(["hold_expired"])
  wait -->|"pay signal"| charge["ChargePayment<br/>key k:charge, retried"]
  charge -->|"declined"| rel2["ReleaseHold"] --> payfail(["payment_failed"])
  charge -->|"charged"| buy["Purchase<br/>key k, retried"]
  buy -->|"order created"| done(["purchased"])
  buy -->|"fails, e.g. hold expired"| refund["RefundPayment"] --> rel3["ReleaseHold"] --> buyfail(["purchase_failed"])
```

> If the charge succeeds but the purchase fails, we refund, then release. Retries are safe because every step that can repeat is keyed.
