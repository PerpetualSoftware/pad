-- Migration 092: app install staging and provenance (SPEC-6 U8a, TASK-3397).
-- Mirrors SQLite migration 118; see it for the rationale.
CREATE TABLE IF NOT EXISTS app_install_pending (
    id                TEXT PRIMARY KEY,
    workspace_id      TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    owner_id          TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    origin            TEXT NOT NULL,
    state             TEXT NOT NULL DEFAULT 'fetching' CHECK (state IN ('fetching', 'staged')),
    reserved_bytes    BIGINT NOT NULL,
    manifest_sha256   TEXT,
    preview           TEXT,
    expires_at        TEXT NOT NULL,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_app_install_pending_owner ON app_install_pending (owner_id, expires_at);
CREATE INDEX IF NOT EXISTS idx_app_install_pending_expires ON app_install_pending (expires_at);

CREATE TABLE IF NOT EXISTS app_install_pending_blobs (
    pending_id TEXT NOT NULL REFERENCES app_install_pending(id) ON DELETE CASCADE,
    key        TEXT NOT NULL,
    url        TEXT NOT NULL,
    sha256     TEXT NOT NULL,
    size_bytes BIGINT NOT NULL,
    data       BYTEA NOT NULL,
    PRIMARY KEY (pending_id, key)
);

ALTER TABLE app_installs ADD COLUMN IF NOT EXISTS bot_user_id TEXT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE app_installs ADD COLUMN IF NOT EXISTS manifest_sha256 TEXT;
ALTER TABLE app_installs ADD COLUMN IF NOT EXISTS manifest_version TEXT;
ALTER TABLE app_installs ADD COLUMN IF NOT EXISTS manifest TEXT;
ALTER TABLE app_installs ADD COLUMN IF NOT EXISTS service_access TEXT;
ALTER TABLE app_installs ADD COLUMN IF NOT EXISTS delegated_access TEXT;
ALTER TABLE app_installs ADD COLUMN IF NOT EXISTS config TEXT NOT NULL DEFAULT '{}';
ALTER TABLE app_installs ADD COLUMN IF NOT EXISTS digests TEXT NOT NULL DEFAULT '{}';

ALTER TABLE collections ADD COLUMN IF NOT EXISTS via_app TEXT REFERENCES app_installs(id) ON DELETE SET NULL;

ALTER TABLE items ADD COLUMN IF NOT EXISTS source_pack TEXT;
ALTER TABLE items ADD COLUMN IF NOT EXISTS source_artifact_sha256 TEXT;
ALTER TABLE items ADD COLUMN IF NOT EXISTS installed_sha256 TEXT;
