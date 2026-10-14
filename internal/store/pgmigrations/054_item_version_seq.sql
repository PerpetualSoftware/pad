-- Add version_seq: a per-item monotonic version counter (BUG-2270).
-- Postgres mirror of migrations/076_item_version_seq.sql. BIGINT because
-- it's a per-item counter that grows unbounded. See the SQLite migration
-- for the full rationale (second-precision created_at ties + random
-- UUIDv4 id = no ordering tie-breaker; rowid is SQLite-only so can't be
-- the cross-dialect fix).
ALTER TABLE item_versions ADD COLUMN IF NOT EXISTS version_seq BIGINT NOT NULL DEFAULT 0;

-- Backfill existing rows deterministically per item: by created_at, then
-- id. Rows written before this migration carry no sub-second chronology
-- (second-precision created_at, random UUIDv4 id), so versions saved in the
-- same second are ordered by id, which is arbitrary but stable. It used to
-- be ctid, which is NOT chronology either: an UPDATE moves a row, so it
-- could order a tie differently on every database (BUG-3751). The runner
-- records applied migrations by filename only (store.go migratePostgres),
-- so databases that already ran this keep their numbering; operators can
-- check theirs with scripts/diagnose-version-ties.
UPDATE item_versions AS v
SET version_seq = sub.rn
FROM (
    SELECT id, ROW_NUMBER() OVER (PARTITION BY item_id ORDER BY created_at, id) AS rn
    FROM item_versions
) AS sub
WHERE v.id = sub.id;

CREATE INDEX IF NOT EXISTS idx_item_versions_item_seq ON item_versions(item_id, version_seq);
