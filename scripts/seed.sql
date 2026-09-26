WITH e AS (
    INSERT INTO events (name, starts_at)
    VALUES ('Demo Game', now() + interval '30 days')
    RETURNING id
)
INSERT INTO seats (event_id, label, price_cents)
SELECT e.id, 'A' || n, 5000 FROM e, generate_series(1, 20) AS n;
