-- Migration 085: item execution lease (#1221).
--
-- Two processes that both read an item's status and conclude "no one else
-- has started this" have no way to make that decision atomically — whatever
-- they do next happens after the read, so both proceed. The lease is a
-- first-class, time-bounded "someone is executing this right now" state,
-- acquired by a conditional UPDATE whose predicate is the arbiter (the
-- protocol migration 083 / TASK-2714 established for the event outbox, and
-- BUG-2415 before that for orphan GC).
--
-- lease_expires_at doubles as the reaper: an expired lease is treated as
-- absent by every read and by the claim predicate, so a crashed holder
-- strands nothing and no sweep job exists. The lease is DELIBERATELY not
-- part of the item's content state — claiming bumps neither updated_at nor
-- the version history, so a lease cannot 409 a concurrent editor's
-- expected_updated_at token.
--
-- NOTE: no `IF NOT EXISTS` — SQLite's ALTER TABLE ADD COLUMN rejects it.
-- Nullable with no default; NULL = unclaimed, so no backfill is needed.
ALTER TABLE items ADD COLUMN lease_holder TEXT;
ALTER TABLE items ADD COLUMN lease_acquired_at TEXT;
ALTER TABLE items ADD COLUMN lease_expires_at TEXT;

-- The workspace-wide live-lease listing (item list decoration) scans by
-- workspace and expiry; the partial index keeps unleased rows out of it.
CREATE INDEX IF NOT EXISTS idx_items_lease
    ON items(workspace_id, lease_expires_at)
    WHERE lease_holder IS NOT NULL;
