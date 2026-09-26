CREATE TABLE events (
    id        bigserial PRIMARY KEY,
    name      text        NOT NULL,
    starts_at timestamptz NOT NULL
);

CREATE TABLE holds (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id   bigint      NOT NULL REFERENCES events (id),
    buyer_id   text        NOT NULL,
    seat_count integer     NOT NULL CHECK (seat_count > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

CREATE TABLE orders (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hold_id         uuid        NOT NULL UNIQUE REFERENCES holds (id),
    buyer_id        text        NOT NULL,
    total_cents     bigint      NOT NULL,
    idempotency_key text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (buyer_id, idempotency_key)
);

-- A seat's state lives on its own row, so one row is the unit of contention.
-- Available means: order_id IS NULL AND (hold_id IS NULL OR hold_expires_at <= now()).
-- Expired holds need no sweeper; every query applies that predicate.
CREATE TABLE seats (
    id              bigserial PRIMARY KEY,
    event_id        bigint  NOT NULL REFERENCES events (id),
    label           text    NOT NULL,
    price_cents     integer NOT NULL CHECK (price_cents >= 0),
    hold_id         uuid REFERENCES holds (id),
    hold_expires_at timestamptz,
    -- Deferred so purchase can claim seats before inserting the order row.
    order_id        uuid REFERENCES orders (id) DEFERRABLE INITIALLY DEFERRED,
    UNIQUE (event_id, label),
    CHECK ((hold_id IS NULL) = (hold_expires_at IS NULL))
);

CREATE INDEX seats_hold_id_idx ON seats (hold_id) WHERE hold_id IS NOT NULL;
CREATE INDEX seats_order_id_idx ON seats (order_id) WHERE order_id IS NOT NULL;
