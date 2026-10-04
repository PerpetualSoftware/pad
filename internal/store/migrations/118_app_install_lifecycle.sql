-- Migration 118: app install staging and provenance (SPEC-6 U8a, TASK-3397).
--
-- app_install_pending is the pending-install record (DOC-3371 §2 step 1): a
-- reservation taken BEFORE any network fetch, under the owner's users row and
-- an instance-wide lock, that charges the full per-install byte cap until the
-- fetch has ended. The staged bytes live in app_install_pending_blobs (the
-- manifest under the key '$manifest', then one row per artifact), so a
-- failure, the fetch deadline or expiry deletes the reservation and every
-- staged byte in one statement (ON DELETE CASCADE). A fetching record expires
-- two minutes after it is taken (the 30 s fetch deadline plus margin), so a
-- process that dies mid-fetch holds its charge only that long; a staged one
-- is kept for an hour awaiting consent.
--
-- The app_installs columns are written by provisioning (U8b): the install's
-- bot (the OAuth client is U5's, keyed by oauth_clients.app_install_id), the manifest's hash and version, the declared events
-- and item actions (stored now, provisioned when A5/A6 land), the access
-- levels, the owner-edited config and every raw and normalized digest.
--
-- collections.via_app is the install that created a companion collection. The
-- conflict check (§2 step 5, L6) adopts an existing slug only when the install
-- that created it has the same origin; any other existing slug is an error.
--
-- items.source_pack / source_artifact_sha256 / installed_sha256 stamp a
-- companion artifact with the pack it came from and both digests (§2 step 6),
-- so a later installer can adopt it.
CREATE TABLE IF NOT EXISTS app_install_pending (
    id                TEXT PRIMARY KEY,
    workspace_id      TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    owner_id          TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    origin            TEXT NOT NULL,
    state             TEXT NOT NULL DEFAULT 'fetching' CHECK (state IN ('fetching', 'staged')),
    reserved_bytes    INTEGER NOT NULL,
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
    size_bytes INTEGER NOT NULL,
    data       BLOB NOT NULL,
    PRIMARY KEY (pending_id, key)
);

ALTER TABLE app_installs ADD COLUMN bot_user_id TEXT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE app_installs ADD COLUMN manifest_sha256 TEXT;
ALTER TABLE app_installs ADD COLUMN manifest_version TEXT;
ALTER TABLE app_installs ADD COLUMN manifest TEXT;
ALTER TABLE app_installs ADD COLUMN service_access TEXT;
ALTER TABLE app_installs ADD COLUMN delegated_access TEXT;
ALTER TABLE app_installs ADD COLUMN config TEXT NOT NULL DEFAULT '{}';
ALTER TABLE app_installs ADD COLUMN digests TEXT NOT NULL DEFAULT '{}';

ALTER TABLE collections ADD COLUMN via_app TEXT REFERENCES app_installs(id) ON DELETE SET NULL;

ALTER TABLE items ADD COLUMN source_pack TEXT;
ALTER TABLE items ADD COLUMN source_artifact_sha256 TEXT;
ALTER TABLE items ADD COLUMN installed_sha256 TEXT;
