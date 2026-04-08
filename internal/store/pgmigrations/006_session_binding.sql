-- Session binding: store IP address and User-Agent hash for session theft detection
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS ip_address TEXT DEFAULT '';
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS ua_hash TEXT DEFAULT '';

-- Invitation code hardening: store hashed codes (128-bit entropy)
ALTER TABLE workspace_invitations ADD COLUMN IF NOT EXISTS code_hash TEXT DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_invitations_code_hash ON workspace_invitations(code_hash);
