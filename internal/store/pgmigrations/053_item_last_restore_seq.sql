-- Add last_restore_seq: the durable per-item restore boundary (BUG-2264).
-- Postgres mirror of migrations/075_item_last_restore_seq.sql. BIGINT because
-- seq is a workspace-monotonic counter that grows unbounded; NULL = never
-- restored, so no backfill is required. See the SQLite migration for the full
-- rationale (durable Join fence that survives a server restart).
ALTER TABLE items ADD COLUMN IF NOT EXISTS last_restore_seq BIGINT;

-- restore_boundary_op_id: DURABLE op-log-id restore boundary (pre-prune
-- MAX(op-log.id)+1). Postgres mirror; makes the collab-snapshot flush gate's
-- restore boundary survive a restart. See the SQLite migration for rationale.
ALTER TABLE items ADD COLUMN IF NOT EXISTS restore_boundary_op_id BIGINT;
