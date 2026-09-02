-- Durable trace context.
--
-- The outbox deliberately decouples ingest from enqueue: ingest writes a row
-- and returns, and a background relay later picks that row up and pushes it to
-- the queue. That decoupling is the whole point — but it also means the relay
-- has no in-process link to the request that created the event. It is a
-- different goroutine, usually a different process, running minutes later.
--
-- So in-memory context propagation cannot span it. Without this column an
-- event's trace ends at ingest, and every delivery attempt opens its own
-- unconnected trace: you can see that an ingest happened and that a delivery
-- happened, with no way to tell they were the same event.
--
-- The W3C carrier is stored as JSONB rather than a bare traceparent TEXT so
-- that tracestate and baggage ride along too, and so adding a future
-- propagation header is not another migration.
--
-- Nullable, because events ingested before this column existed have no context,
-- and because tracing can legitimately be disabled. A NULL simply means the
-- delivery starts a fresh root.

BEGIN;

ALTER TABLE events
    ADD COLUMN trace_context JSONB;

COMMIT;
