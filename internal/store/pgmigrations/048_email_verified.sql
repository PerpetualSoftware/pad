-- Add email_verified_at column for email verification (PLAN-1933 Wave 1 / TASK-1935).
-- NULL = unverified, non-NULL = the time the email was verified. Mirrors
-- disabled_at's nullable-timestamp shape. TEXT (not TIMESTAMP) to match the
-- codebase's RFC3339-string convention for user timestamps. Pure infra —
-- nothing reads this column until Wave 3.
--
-- The SAFE default (verified) is enforced in the application layer
-- (store.CreateUser / CreateOAuthUser), NOT via a DB DEFAULT, so both dialects
-- stay symmetric and the fail-safe lives in one testable place.
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verified_at TEXT;

-- Backfill: UNCONDITIONALLY mark EVERY existing row verified. Existing / OAuth /
-- self-host accounts predate email verification and must NOT be write-locked on
-- deploy. This is INVERTED vs password_set's conditional (oauth-aware) backfill.
-- Emit RFC3339 UTC with a 'Z' suffix so Go's time.Parse(time.RFC3339, …) reads
-- it back. now() AT TIME ZONE 'UTC' yields a naive UTC timestamp that to_char
-- formats as-is, and the hardcoded 'Z' labels it UTC regardless of server
-- locale (the plain ::text cast would be space-separated and un-parseable).
UPDATE users
SET email_verified_at = to_char(
    now() AT TIME ZONE 'UTC',
    'YYYY-MM-DD"T"HH24:MI:SS"Z"'
)
WHERE email_verified_at IS NULL;
