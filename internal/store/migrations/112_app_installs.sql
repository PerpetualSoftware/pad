-- Migration 112: app installs (SPEC-6 U1, TASK-3388).
--
-- One row per app installed in one workspace: the unit of consent,
-- credentials, epoch, quota and revocation (DOC-3371 §2). U1 creates only
-- what the write fence reads. Every app mutation transaction opens by
-- reading (auth_epoch, state) here, under a shared lock on Postgres, and
-- refuses unless the epoch it was admitted under is current and the install
-- is active. Disable, rotate and uninstall take the row exclusively and bump
-- the epoch, so no app write commits after them. Later units add the client,
-- bot, manifest and usage columns. An uninstalled install is never deleted:
-- it stays as the tombstone that via_app attribution points at.
CREATE TABLE IF NOT EXISTS app_installs (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    origin       TEXT NOT NULL,
    state        TEXT NOT NULL DEFAULT 'active'
                 CHECK (state IN ('active', 'disabling', 'inactive', 'uninstalling', 'uninstalled')),
    auth_epoch   INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_app_installs_workspace ON app_installs (workspace_id);
