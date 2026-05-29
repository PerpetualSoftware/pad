-- Migration 044 (Postgres): monotonic ordering column for status_transitions
-- (PLAN-1628 / TASK-1643). Postgres counterpart to
-- migrations/065_status_transitions_seq.sql.
--
-- `seq` (MAX(seq)+1 at insert, like items.seq) gives a stable insertion-order
-- tiebreak for the as-of-T snapshot's "latest transition <= T" resolution,
-- which is otherwise nondeterministic for 2+ same-second hops on one item.
-- Backfill existing rows chronologically; new inserts continue from MAX(seq).

ALTER TABLE status_transitions ADD COLUMN IF NOT EXISTS seq INTEGER NOT NULL DEFAULT 0;

WITH ordered AS (
    SELECT id, ROW_NUMBER() OVER (ORDER BY created_at, id) AS rn
    FROM status_transitions
)
UPDATE status_transitions
SET seq = ordered.rn
FROM ordered
WHERE ordered.id = status_transitions.id;
