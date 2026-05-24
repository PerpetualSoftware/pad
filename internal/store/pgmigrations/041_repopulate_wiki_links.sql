-- Postgres mirror of SQLite migration 062 (PLAN-1593 / TASK-1595).
-- See internal/store/migrations/062_repopulate_wiki_links.sql for the full
-- rationale; this file diverges only where engine syntax requires it.
--
-- Differences from the SQLite migration:
--   * No syntax differences here — DELETE FROM is identical in both engines.
--     Kept as a separate file so the migration numbering stays in lockstep
--     with the rest of the schema.

DELETE FROM item_wiki_links;
