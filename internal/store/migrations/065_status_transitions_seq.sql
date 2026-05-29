-- Migration 065: monotonic ordering column for status_transitions
-- (PLAN-1628 / TASK-1643).
--
-- created_at is second-precision and ids are random UUIDs, so resolving "the
-- latest transition <= T" for the as-of historical snapshot is nondeterministic
-- when an item changes status 2+ times within ONE second. A monotonic `seq`
-- (assigned MAX(seq)+1 at insert, like items.seq) gives a stable insertion-order
-- tiebreak. Backfill existing rows in chronological order so legacy data orders
-- correctly too; new inserts continue from MAX(seq).

ALTER TABLE status_transitions ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;

WITH ordered AS (
    SELECT id, ROW_NUMBER() OVER (ORDER BY created_at, id) AS rn
    FROM status_transitions
)
UPDATE status_transitions
SET seq = (SELECT rn FROM ordered WHERE ordered.id = status_transitions.id);
