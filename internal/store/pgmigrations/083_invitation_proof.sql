-- Migration 083: a mailbox-only proof on workspace invitations (TASK-3352).
-- Mirrors SQLite migration 109; see it for the rationale.
ALTER TABLE workspace_invitations ADD COLUMN IF NOT EXISTS proof_hash TEXT NOT NULL DEFAULT '';
