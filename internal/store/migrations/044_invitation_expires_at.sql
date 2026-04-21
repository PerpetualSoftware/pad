-- Add expires_at to workspace_invitations so stale join codes stop working.
-- Without expiry, a one-time invite code lives forever until accepted — if it
-- leaks (email forwarding, stale screenshot, git history) an attacker who
-- registers the invitee's email can still claim the workspace seat months
-- or years later.
--
-- Default window is 14 days, enforced in the Go handlers. Existing rows are
-- backfilled relative to their created_at so they also age out.
ALTER TABLE workspace_invitations ADD COLUMN expires_at TEXT;

-- Backfill: 14 days after the creation time, emitted in RFC3339 with a 'Z'
-- suffix. We must match the format used by Go's time.RFC3339 exactly —
-- internal/store/store.go's parseTime silently returns the zero Time on
-- parse failure, which would make IsExpired() treat every legacy invitation
-- as already-expired right after the migration runs. The default
-- datetime(..., '+14 days') output of 'YYYY-MM-DD HH:MM:SS' is space-separated
-- and un-parseable by RFC3339, so we strftime the result into the expected
-- shape.
UPDATE workspace_invitations
SET expires_at = strftime('%Y-%m-%dT%H:%M:%SZ', created_at, '+14 days')
WHERE expires_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_invitations_expires_at ON workspace_invitations(expires_at);
