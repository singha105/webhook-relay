-- Reverses 000001_init.up.sql. Dropped in dependency order: children before
-- parents, and the trigger function last since the trigger that uses it dies
-- with its table.
BEGIN;

DROP TABLE IF EXISTS delivery_attempts;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS endpoints;
DROP FUNCTION IF EXISTS set_updated_at();

COMMIT;
