-- Index only; no columns and no data change.
--
-- webhook_queue_oldest_message_age_seconds is the SLO metric, and it is
-- computed as now() - min(created_at) over every event that has not reached a
-- terminal state. Prometheus scrapes it every 15 seconds, and /readyz reads it
-- on every probe, so it must not be a sequential scan.
--
-- The existing events_due_idx cannot serve it: that index is keyed on
-- next_retry_at, so finding the minimum created_at through it would mean
-- reading every non-terminal row.
--
-- Partial on the same predicate as the other work-set indexes, so it tracks the
-- size of the backlog rather than lifetime volume — and min() over it is a
-- single index lookup at the leftmost leaf.

BEGIN;

CREATE INDEX events_backlog_age_idx
    ON events (created_at)
    WHERE status IN ('pending', 'failed', 'delivering');

COMMIT;
