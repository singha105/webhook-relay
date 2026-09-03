-- webhook-relay initial schema.
--
-- Three tables: endpoints (delivery destinations), events (one webhook bound
-- for one endpoint), delivery_attempts (append-only audit trail of HTTP calls).
--
-- Every index below is justified inline. An unjustified index is a write
-- amplifier: this system is write-heavy on the ingest path, so each extra
-- index is paid for on every single INSERT.

BEGIN;

-- ---------------------------------------------------------------------------
-- endpoints
-- ---------------------------------------------------------------------------
CREATE TABLE endpoints (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    url                  TEXT        NOT NULL,
    description          TEXT        NOT NULL DEFAULT '',

    -- Stored in plaintext because the delivery worker must recompute an HMAC
    -- over the payload at send time. Unlike a password this cannot be a one-way
    -- hash. The "shown once" guarantee is enforced at the API boundary: the
    -- column is never selected into any response after creation.
    signing_secret       TEXT        NOT NULL,

    is_active            BOOLEAN     NOT NULL DEFAULT TRUE,
    rate_limit_per_sec   INTEGER     NOT NULL DEFAULT 10,

    -- Populated by the Day 3 circuit breaker. Added now, with a default, so
    -- turning the breaker on later is a code change rather than an ALTER TABLE
    -- against a table that already holds rows.
    consecutive_failures INTEGER     NOT NULL DEFAULT 0,

    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT endpoints_url_not_blank
        CHECK (length(btrim(url)) > 0),
    CONSTRAINT endpoints_url_len
        CHECK (length(url) <= 2048),
    CONSTRAINT endpoints_rate_limit_range
        CHECK (rate_limit_per_sec BETWEEN 1 AND 1000),
    CONSTRAINT endpoints_consecutive_failures_nonneg
        CHECK (consecutive_failures >= 0)
);

-- Keeps updated_at honest even for hand-written SQL run during an incident.
-- Doing this in the application would mean every future writer has to remember.
CREATE FUNCTION set_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER endpoints_set_updated_at
    BEFORE UPDATE ON endpoints
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- events
-- ---------------------------------------------------------------------------
CREATE TABLE events (
    -- No DEFAULT: the application supplies a UUIDv7 so IDs are time-ordered
    -- and inserts append to the right edge of the primary-key B-tree. A v4
    -- default here would give us random-page writes for no benefit.
    id              UUID        PRIMARY KEY,

    -- ON DELETE CASCADE: an endpoint's events are meaningless without it, and
    -- we would rather delete them than accumulate orphans pointing at a
    -- destination we can no longer resolve.
    endpoint_id     UUID        NOT NULL REFERENCES endpoints (id) ON DELETE CASCADE,

    event_type      TEXT        NOT NULL,
    payload         JSONB       NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'pending',
    idempotency_key TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT events_status_valid
        CHECK (status IN ('pending', 'delivering', 'delivered', 'failed', 'dlq')),
    CONSTRAINT events_event_type_not_blank
        CHECK (length(btrim(event_type)) > 0),
    CONSTRAINT events_event_type_len
        CHECK (length(event_type) <= 128),
    CONSTRAINT events_idempotency_key_len
        CHECK (idempotency_key IS NULL OR length(idempotency_key) <= 255),
    CONSTRAINT events_payload_size
        CHECK (pg_column_size(payload) <= 262144)
);

-- INDEX 1 — idempotency.
-- This is a correctness constraint, not a performance index. It is the thing
-- that makes concurrent duplicate ingests safe: we INSERT unconditionally and
-- let the database reject the loser, rather than SELECT-then-INSERT, which has
-- a race window between the two statements no amount of application locking
-- closes cheaply.
-- Partial (WHERE NOT NULL) because the vast majority of events carry no key.
-- Postgres treats NULLs as distinct in a plain UNIQUE, so a full index would
-- work too, but it would be far larger for no added guarantee.
-- Scoped to (endpoint_id, key) so two tenants can both use "order-123".
CREATE UNIQUE INDEX events_endpoint_idempotency_key_uniq
    ON events (endpoint_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- INDEX 2 — the required "find pending events for an endpoint, oldest first".
-- Column order matters: endpoint_id is the equality predicate so it leads,
-- created_at follows so the index supplies the ORDER BY and the planner can
-- stop after LIMIT n instead of sorting the whole match set.
-- Partial on status='pending' for two reasons: the index stays proportional to
-- the *backlog* rather than to lifetime event volume, and a delivered row drops
-- out of it entirely, so the hot index stays small enough to live in cache.
CREATE INDEX events_pending_by_endpoint_idx
    ON events (endpoint_id, created_at)
    WHERE status = 'pending';

-- INDEX 3 — the worker's global claim query for Day 2, which pulls the oldest
-- pending work across all endpoints rather than for one endpoint. Index 2
-- cannot serve this: its leading column is endpoint_id, so an endpoint-less
-- query would have to scan every distinct endpoint value.
-- Added now because the table is empty; building it later against a live table
-- would need CREATE INDEX CONCURRENTLY and a maintenance window.
CREATE INDEX events_pending_by_created_at_idx
    ON events (created_at)
    WHERE status = 'pending';

-- Deliberately NOT indexed:
--   * status alone — only five distinct values, so the planner would pick a
--     sequential scan anyway. The partial indexes above already encode it.
--   * payload (GIN) — nothing queries inside the JSON today. A GIN index is
--     the most expensive kind to maintain on write, and this is the write path.
--   * event_type — no query filters on it yet. Add it when a query needs it.

-- ---------------------------------------------------------------------------
-- delivery_attempts
-- ---------------------------------------------------------------------------
CREATE TABLE delivery_attempts (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id       UUID        NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    attempt_number INTEGER     NOT NULL,

    -- NULL when the request never produced an HTTP response at all: DNS
    -- failure, connection refused, or timeout. Day 3's retry policy treats
    -- "no response" differently from a real 5xx, so the distinction has to
    -- survive in storage.
    status_code    INTEGER,

    response_body  TEXT        NOT NULL DEFAULT '',
    error_message  TEXT,
    duration_ms    INTEGER     NOT NULL,
    attempted_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT delivery_attempts_number_positive
        CHECK (attempt_number >= 1),
    CONSTRAINT delivery_attempts_duration_nonneg
        CHECK (duration_ms >= 0),
    -- Mirrors models.MaxResponseBodyBytes. The application truncates; this is
    -- the backstop that keeps a future code path from turning this column into
    -- a log sink.
    CONSTRAINT delivery_attempts_body_size
        CHECK (octet_length(response_body) <= 2048),
    CONSTRAINT delivery_attempts_status_code_range
        CHECK (status_code IS NULL OR status_code BETWEEN 100 AND 599),

    -- INDEX 4 — doubles as a correctness guard and the read path.
    -- As a constraint it makes attempt numbering idempotent: a worker that
    -- crashes after writing attempt 3 and retries cannot write a second row 3.
    -- As an index it serves GET /v1/events/{id}, which fetches every attempt
    -- for one event ordered by attempt_number. That is exactly this index's
    -- leading column and sort order, so no separate index on event_id is
    -- needed — adding one would be pure write overhead.
    CONSTRAINT delivery_attempts_event_attempt_uniq
        UNIQUE (event_id, attempt_number)
);

COMMIT;
