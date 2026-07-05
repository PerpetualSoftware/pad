-- Migration 072: activities.user_id -> ON DELETE SET NULL (TASK-1959).
--
-- activities is pad's append-only audit/history log. It records every
-- action, INCLUDING the session_ip_changed audit rows the auth
-- middleware writes on the very request that deletes an account (a
-- delete from a changed IP). Its user_id FK previously carried no
-- ON DELETE action, so DELETE FROM users could 500 with nothing deleted
-- whenever an activity row still referenced the user — the exact FK gap
-- the delete-account tests worked around by pinning RemoteAddr and
-- scrubbing activities.user_id.
--
-- ON DELETE SET NULL preserves the audit row and drops the identity —
-- the same posture comments.user_id uses (TASK-509). Because
-- activities is written on nearly every request, the schema-level SET
-- NULL also closes the race where a concurrent request logs an activity
-- for the user between DeleteAccountAtomic's cleanup and its final
-- DELETE FROM users.
--
-- SQLite can't ALTER a column-level FK in place, so we rebuild the table
-- following the 022_audit_trail pattern: PKs are preserved, so
-- comments.activity_id references stay valid; FK enforcement is disabled
-- during the swap. The Postgres side (pgmigrations/050) adds the same
-- constraint via ALTER TABLE — Postgres never had a FK on this column.

PRAGMA foreign_keys = OFF;

DROP TABLE IF EXISTS activities_new;

CREATE TABLE activities_new (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT REFERENCES workspaces(id),
    document_id   TEXT,
    action        TEXT NOT NULL,
    actor         TEXT NOT NULL,
    source        TEXT NOT NULL DEFAULT 'web',
    metadata      TEXT NOT NULL DEFAULT '{}',
    created_at    TEXT NOT NULL,
    user_id       TEXT REFERENCES users(id) ON DELETE SET NULL,
    ip_address    TEXT,
    user_agent    TEXT
);

INSERT INTO activities_new (id, workspace_id, document_id, action, actor, source, metadata, created_at, user_id, ip_address, user_agent)
SELECT id, workspace_id, document_id, action, actor, source, metadata, created_at, user_id, ip_address, user_agent
FROM activities;

DROP TABLE activities;
ALTER TABLE activities_new RENAME TO activities;

-- Recreate indexes (mirror of 022_audit_trail.sql).
CREATE INDEX IF NOT EXISTS idx_activities_workspace ON activities(workspace_id, created_at);
CREATE INDEX IF NOT EXISTS idx_activities_document ON activities(document_id, created_at);
CREATE INDEX IF NOT EXISTS idx_activities_action ON activities(action, created_at);
CREATE INDEX IF NOT EXISTS idx_activities_user ON activities(user_id, created_at);

PRAGMA foreign_keys = ON;
