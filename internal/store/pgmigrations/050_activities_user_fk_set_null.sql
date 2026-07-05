-- Migration 050: add activities.user_id -> users(id) ON DELETE SET NULL (TASK-1959).
--
-- Postgres never enforced a FK on activities.user_id (SQLite did). Adding
-- it with ON DELETE SET NULL brings the two dialects to parity and makes
-- DELETE FROM users de-identify audit/history rows instead of leaving a
-- dangling user_id behind. This mirrors the SQLite side (migrations/072)
-- and is the durable form of the "ON DELETE SET NULL behaviour" the
-- delete-account tests previously simulated with a manual scrub.
-- See TASK-1959 for the full FK audit.
--
-- Defensive orphan scrub first: any activities.user_id that no longer
-- references a live user (possible precisely because Postgres had no FK
-- to enforce it) is nulled so ADD CONSTRAINT validation passes.

UPDATE activities SET user_id = NULL
WHERE user_id IS NOT NULL
  AND user_id NOT IN (SELECT id FROM users);

ALTER TABLE activities DROP CONSTRAINT IF EXISTS activities_user_id_fkey;
ALTER TABLE activities ADD CONSTRAINT activities_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL;
