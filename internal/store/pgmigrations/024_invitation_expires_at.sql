-- Add expires_at to workspace_invitations so stale join codes stop working.
-- Without expiry, a one-time invite code lives forever until accepted — if it
-- leaks (email forwarding, stale screenshot, git history) an attacker who
-- registers the invitee's email can still claim the workspace seat months
-- or years later.
--
-- Default window is 14 days, enforced in the Go handlers. Existing rows are
-- backfilled relative to their created_at so they also age out.
ALTER TABLE workspace_invitations ADD COLUMN IF NOT EXISTS expires_at TEXT;

-- Backfill: 14 days after the creation time, emitted in RFC3339 with a 'Z'
-- suffix so Go's time.Parse(time.RFC3339, …) can read the result. The default
-- ::TEXT cast of a timestamp produces space-separated output that parseTime
-- (internal/store/store.go) silently fails on, which would make IsExpired()
-- treat every legacy invitation as already-expired right after migration.
--
-- We do the interval math on the NAIVE timestamp (no AT TIME ZONE conversion).
-- created_at is stored as RFC3339 UTC text, so `created_at::timestamp` gives
-- a timestamp-without-tz whose numeric value is already UTC. to_char on a
-- plain timestamp uses the timestamp as-is — it does NOT apply the session's
-- TimeZone setting (which would happen for timestamptz). The hardcoded 'Z'
-- suffix then correctly labels the result as UTC regardless of server locale.
UPDATE workspace_invitations
SET expires_at = to_char(
    created_at::timestamp + INTERVAL '14 days',
    'YYYY-MM-DD"T"HH24:MI:SS"Z"'
)
WHERE expires_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_invitations_expires_at ON workspace_invitations(expires_at);
