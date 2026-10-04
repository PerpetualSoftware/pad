-- Migration 093: app install codes (SPEC-6 U8b, TASK-3397).
-- Mirrors SQLite migration 119; see it for the rationale.
CREATE TABLE IF NOT EXISTS app_install_codes (
    code_sha256 TEXT PRIMARY KEY,
    install_id  TEXT NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
    expires_at  TEXT NOT NULL,
    consumed_at TEXT,
    created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_app_install_codes_install ON app_install_codes (install_id);
