-- Migration 119: app install codes (SPEC-6 U8b, TASK-3397).
--
-- Provisioning mints a one-time install code the owner hands to the app
-- (DOC-3371 §2 step 8). Only its SHA-256 is stored. It is bound to one
-- install, expires ten minutes after it is minted, and is consumed exactly
-- once by POST /api/app/v1/install/redeem, which rotates the install
-- client's secret and returns it. The owner can mint a new code to recover
-- a lost redeem response; redeeming it rotates again.
CREATE TABLE IF NOT EXISTS app_install_codes (
    code_sha256 TEXT PRIMARY KEY,
    install_id  TEXT NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
    expires_at  TEXT NOT NULL,
    consumed_at TEXT,
    created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_app_install_codes_install ON app_install_codes (install_id);
