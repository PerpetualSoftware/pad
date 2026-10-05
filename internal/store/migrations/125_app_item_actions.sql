-- Migration 125: app item actions and context codes (SPEC-6 U11, DOC-3371
-- §6; TASK-3414).
--
-- app_item_actions: one row per install and action key, written from the
-- install's manifest at provisioning and by a reviewed upgrade. revision
-- increments whenever an upgrade changes the action (label, path or
-- collections); a removed action is kept with active = 0, so a context code
-- minted before the upgrade names an action that no longer applies and is
-- refused. collection_ids is the JSON array of companion collection ids
-- the action is offered on.
CREATE TABLE IF NOT EXISTS app_item_actions (
    id              TEXT PRIMARY KEY,
    install_id      TEXT NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
    action_key      TEXT NOT NULL,
    label           TEXT NOT NULL,
    path            TEXT NOT NULL,
    collection_ids  TEXT NOT NULL,
    revision        INTEGER NOT NULL DEFAULT 1,
    active          INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    UNIQUE (install_id, action_key)
);

-- app_context_codes: a link-out's single-use code. Only its sha256 is
-- stored. Bound to the install, its auth_epoch, the item, the action and its
-- revision, and the viewer who minted it. Valid two minutes; consumed by the
-- first redeem whatever its outcome. expires_at is database time, written
-- and compared in SQL.
CREATE TABLE IF NOT EXISTS app_context_codes (
    code_sha256     TEXT PRIMARY KEY,
    install_id      TEXT NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
    auth_epoch      INTEGER NOT NULL,
    item_id         TEXT NOT NULL,
    action_id       TEXT NOT NULL,
    action_revision INTEGER NOT NULL,
    viewer_user_id  TEXT NOT NULL,
    expires_at      TEXT NOT NULL,
    consumed_at     TEXT
);
CREATE INDEX IF NOT EXISTS idx_app_context_codes_expiry ON app_context_codes (expires_at);
