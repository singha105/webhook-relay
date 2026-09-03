-- Reverses 000002. Restores the day-1 indexes so a rollback leaves the schema
-- exactly as 000001 created it, not merely functional.
BEGIN;

DROP INDEX IF EXISTS events_due_by_endpoint_idx;
DROP INDEX IF EXISTS events_due_idx;

CREATE INDEX events_pending_by_endpoint_idx
    ON events (endpoint_id, created_at)
    WHERE status = 'pending';

CREATE INDEX events_pending_by_created_at_idx
    ON events (created_at)
    WHERE status = 'pending';

ALTER TABLE events
    DROP CONSTRAINT IF EXISTS events_attempt_count_nonneg,
    DROP COLUMN IF EXISTS next_retry_at,
    DROP COLUMN IF EXISTS attempt_count;

COMMIT;
