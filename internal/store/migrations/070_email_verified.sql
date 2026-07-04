-- Add email_verified_at column for email verification (PLAN-1933 Wave 1 / TASK-1935).
-- NULL = unverified, non-NULL = the time the email was verified. Mirrors
-- disabled_at's nullable-timestamp shape. Pure infra — nothing reads this column
-- until Wave 3, so this migration is a no-op behaviourally.
--
-- NOTE: no `IF NOT EXISTS` — SQLite's ALTER TABLE ADD COLUMN rejects it.
-- NOTE: no expression DEFAULT either — SQLite forbids a parenthesised/CURRENT_*
-- default on ADD COLUMN, so the column defaults to NULL. The SAFE default
-- (verified) is enforced in the application layer instead: store.CreateUser /
-- CreateOAuthUser write a verified timestamp unless a creation path explicitly
-- requests unverified. The ONLY path that will ever leave this NULL is the
-- future cloud self-serve signup branch (Wave 3), which does not exist yet.
ALTER TABLE users ADD COLUMN email_verified_at TEXT;

-- Backfill: UNCONDITIONALLY mark EVERY existing row verified. Existing / OAuth /
-- self-host accounts predate email verification and must NOT be write-locked on
-- deploy. This is INVERTED vs password_set's conditional (oauth-aware) backfill:
-- there is no "was this user ever verified?" signal to key on, and the correct
-- answer for every pre-existing account is "verified". Emit RFC3339 with a 'Z'
-- suffix so Go's time.Parse(time.RFC3339, …) / store.parseTime reads it back
-- (the default datetime(...) 'YYYY-MM-DD HH:MM:SS' output is space-separated and
-- un-parseable by RFC3339) — same convention as disabled_at / created_at.
UPDATE users
SET email_verified_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
WHERE email_verified_at IS NULL;
