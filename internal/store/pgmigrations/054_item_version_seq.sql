-- Add version_seq: a per-item monotonic version counter (BUG-2270).
-- Postgres mirror of migrations/076_item_version_seq.sql. BIGINT because
-- it's a per-item counter that grows unbounded. See the SQLite migration
-- for the full rationale (second-precision created_at ties + random
-- UUIDv4 id = no ordering tie-breaker; rowid is SQLite-only so can't be
-- the cross-dialect fix).
ALTER TABLE item_versions ADD COLUMN IF NOT EXISTS version_seq BIGINT NOT NULL DEFAULT 0;

-- Backfill existing rows deterministically per item. Postgres lacks rowid,
-- so ctid (the physical row locator) is the stable per-partition
-- tie-breaker for same-second rows — the analogue of the SQLite migration's
-- rowid ordering.
UPDATE item_versions AS v
SET version_seq = sub.rn
FROM (
    SELECT id, ROW_NUMBER() OVER (PARTITION BY item_id ORDER BY created_at, ctid) AS rn
    FROM item_versions
) AS sub
WHERE v.id = sub.id;

CREATE INDEX IF NOT EXISTS idx_item_versions_item_seq ON item_versions(item_id, version_seq);
