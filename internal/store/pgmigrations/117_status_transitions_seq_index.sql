-- Migration 117 (Postgres): index status_transitions(seq) (BUG-3667).
-- Postgres counterpart to migrations/147_status_transitions_seq_index.sql; the rationale is there.
CREATE INDEX IF NOT EXISTS idx_status_transitions_seq ON status_transitions(seq);
