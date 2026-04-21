-- Add expires_at to workspace_invitations so stale join codes stop working.
-- Without expiry, a one-time invite code lives forever until accepted — if it
-- leaks (email forwarding, stale screenshot, git history) an attacker who
-- registers the invitee's email can still claim the workspace seat months
-- or years later.
--
-- Default window is 14 days, enforced in the Go handlers. Existing rows are
-- backfilled relative to their created_at so they also age out.
ALTER TABLE workspace_invitations ADD COLUMN expires_at TEXT;

-- Backfill: 14 days after the creation time. SQLite supports datetime arithmetic
-- via the modifiers passed to datetime().
UPDATE workspace_invitations
SET expires_at = datetime(created_at, '+14 days')
WHERE expires_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_invitations_expires_at ON workspace_invitations(expires_at);
