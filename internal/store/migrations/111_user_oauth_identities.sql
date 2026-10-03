-- Migration 111: provider account subjects (TASK-3351).
--
-- A linked sign-in provider was recorded by NAME only (users.oauth_providers),
-- so "google" meant whichever Google account asserted the address. Email is
-- not a stable identity at a provider: an address can move between accounts.
-- Each row binds one provider account (its stable subject) to one pad
-- account. The primary key keeps a provider account on one pad account; the
-- unique index keeps one subject per provider per pad account. Rows are
-- written on the next sign-in that carries a subject; an account without one
-- signs in exactly as before.
CREATE TABLE IF NOT EXISTS user_oauth_identities (
    provider   TEXT NOT NULL,
    subject    TEXT NOT NULL,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    PRIMARY KEY (provider, subject)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_user_oauth_identities_user_provider
    ON user_oauth_identities (user_id, provider);
