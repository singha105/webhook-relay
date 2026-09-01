-- Day 2: durable retry scheduling.
--
-- Retry state lives in Postgres, not in Valkey. The stream is a transport for
-- work that is *ready now*; it is not the system of record. That distinction is
-- what makes Day 5 survivable — Chaos Mesh can delete the Valkey pod outright
-- and every scheduled retry is still on disk here, waiting for the relay to
-- pick it up again. A sorted set in Valkey would have been one less migration
-- and would have silently lost every in-flight retry at the same moment.

BEGIN;

ALTER TABLE events
    -- Denormalized from delivery_attempts. The relay's polling query runs
    -- constantly and would otherwise need a correlated max(attempt_number)
    -- subquery per candidate row. The two are kept consistent in one
    -- transaction by the worker, and a mismatch is only ever an over-count,
    -- which fails safe: the event retries fewer times, never more.
    ADD COLUMN attempt_count INTEGER NOT NULL DEFAULT 0,

    -- Serves two purposes, which is why it is NOT NULL with a now() default
    -- rather than nullable:
    --
    --   1. For pending/failed events it is "do not deliver before this time".
    --      A brand new event defaults to now(), so it is immediately due, and
    --      the relay needs no COALESCE — meaning ORDER BY next_retry_at can be
    --      served directly by an index instead of sorting.
    --
    --   2. For events in 'delivering' it is a LEASE EXPIRY. The relay stamps
    --      now() + lease when it hands an event to the stream. If a worker dies
    --      holding the event and the stream entry is lost too (Valkey restarted,
    --      consumer group gone), nothing would ever redeliver it. The expired
    --      lease is what lets the relay notice and requeue it.
    ADD COLUMN next_retry_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    ADD CONSTRAINT events_attempt_count_nonneg CHECK (attempt_count >= 0);

-- The day-1 partial indexes were predicated on status = 'pending' alone, which
-- no longer describes the work set: an event awaiting its third retry sits in
-- 'failed', and one whose lease has expired sits in 'delivering'. Both must be
-- findable, so both indexes are replaced rather than supplemented — leaving the
-- originals would mean paying for four indexes to answer two questions.
DROP INDEX events_pending_by_endpoint_idx;
DROP INDEX events_pending_by_created_at_idx;

-- INDEX 5 — the relay's global claim, and the stuck-lease sweeper.
-- Leading (and only) column is next_retry_at because that is both the range
-- predicate and the sort order, so the plan is an index scan with no sort node
-- and LIMIT short-circuits it.
-- The partial predicate excludes terminal states, so delivered events and
-- anything in the DLQ drop out of the index entirely. In steady state this
-- index tracks the size of the backlog, not lifetime volume.
CREATE INDEX events_due_idx
    ON events (next_retry_at)
    WHERE status IN ('pending', 'failed', 'delivering');

-- INDEX 6 — the same question scoped to one endpoint, which is what the
-- per-endpoint rate limiter and the operator-facing queries need. endpoint_id
-- leads because it is the equality predicate.
CREATE INDEX events_due_by_endpoint_idx
    ON events (endpoint_id, next_retry_at)
    WHERE status IN ('pending', 'failed', 'delivering');

COMMIT;
