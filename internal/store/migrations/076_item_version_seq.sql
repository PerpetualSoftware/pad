-- Add version_seq: a per-item monotonic version counter that gives
-- item_versions a deterministic tie-breaker (BUG-2270).
--
-- item_versions.created_at is second-precision RFC3339. Two versions
-- minted in the same wall-clock second (a version RESTORE plus rapid
-- edits both do this) tie on created_at, and the id PK is a random
-- UUIDv4 — useless as an ordering tie-breaker. SQLite has rowid but
-- Postgres does not, so rowid can't be the cross-dialect fix. Result:
-- the newest→oldest reconstruction walk (ListItemVersions /
-- ListItemVersionsResolved) resolved same-second versions in arbitrary
-- order, corrupting reconstructed history and "latest".
--
-- version_seq is assigned COALESCE(MAX(version_seq),0)+1 per item in the
-- SAME transaction as each version INSERT. Version creation is serialized
-- per item under the item lock, so MAX+1 is race-safe. The ORDER BYs
-- become `created_at DESC, version_seq DESC` (newest-first) and
-- `created_at, version_seq` (export, oldest-first).
--
-- NOTE: no `IF NOT EXISTS` — SQLite's ALTER TABLE ADD COLUMN rejects it.
ALTER TABLE item_versions ADD COLUMN version_seq INTEGER NOT NULL DEFAULT 0;

-- Backfill existing rows deterministically per item. ROW_NUMBER over
-- (created_at, rowid) reproduces insertion order: rowid is monotonic with
-- insertion for SQLite's implicit rowid, so same-second rows keep the
-- order they were written. Window functions work on SQLite >= 3.25;
-- modernc.org/sqlite bundles a far newer engine.
WITH ranked AS (
    SELECT id AS vid,
           ROW_NUMBER() OVER (PARTITION BY item_id ORDER BY created_at, rowid) AS rn
    FROM item_versions
)
UPDATE item_versions
SET version_seq = (SELECT rn FROM ranked WHERE ranked.vid = item_versions.id);

CREATE INDEX IF NOT EXISTS idx_item_versions_item_seq ON item_versions(item_id, version_seq);
