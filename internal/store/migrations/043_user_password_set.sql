-- Track whether the user has explicitly set a password (vs. the random
-- placeholder hash given to OAuth users in CreateOAuthUser). Used by the
-- OAuth unlink flow to decide whether the user will still have a way to
-- sign in after removing their last linked provider.
ALTER TABLE users ADD COLUMN password_set INTEGER NOT NULL DEFAULT 0;

-- Backfill: any user with no linked OAuth providers must have registered
-- via email/password, so they have a usable password.
UPDATE users
SET password_set = 1
WHERE oauth_providers IS NULL
   OR oauth_providers = ''
   OR oauth_providers = '[]';
