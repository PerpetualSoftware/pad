-- Migration 053: items.seq + workspace-scoped monotonic counter (TASK-1352).
--
-- Adds a per-workspace monotonically-increasing `seq` column that bumps on
-- every items mutation (create / update / soft-delete / restore). Foundation
-- for the local-first read model's delta-sync cursor (PLAN-1343,
-- DOC-1342 design decision #1).
--
-- Robust against the clock-skew / same-millisecond-write / NTP-step
-- correctness holes that an `updated_at` watermark would carry. Each
-- mutation reads `MAX(seq) + 1 WHERE workspace_id = ?` inside the same
-- transaction that performs the write; SQLite's single-writer rule
-- makes that collision-free without an extra lock.
--
-- `seq` is workspace-scoped, NOT global — each workspace has its own
-- monotonic counter so a busy workspace can't fragment another's range
-- and so cursors from one workspace can't accidentally apply to
-- another.

ALTER TABLE items ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;

-- Backfill: assign every existing row a seq in (workspace_id,
-- updated_at, id) order. ROW_NUMBER() OVER PARTITION gives each
-- workspace its own 1..N sequence so the post-migration MAX(seq) per
-- workspace == the row count of that workspace. SQLite supports
-- UPDATE..FROM since 3.33, which is well below the minimum version
-- the rest of the migrations already require.
UPDATE items
SET seq = sub.rn
FROM (
    SELECT id,
           ROW_NUMBER() OVER (PARTITION BY workspace_id ORDER BY updated_at, id) AS rn
    FROM items
) AS sub
WHERE items.id = sub.id;

-- Supports two read patterns:
--   * /items-index cursor read: MAX(seq) per workspace
--   * /items-changes?since= range scan: seq > ? ORDER BY seq ASC
-- DESC is chosen to match the MAX read; the planner can still use the
-- index for the ASC range scan because the column is sorted on the
-- workspace_id leading edge.
CREATE INDEX IF NOT EXISTS idx_items_workspace_seq ON items(workspace_id, seq DESC);
