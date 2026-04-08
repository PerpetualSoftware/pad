-- Session binding: store IP address and User-Agent hash for session theft detection
ALTER TABLE sessions ADD COLUMN ip_address TEXT DEFAULT '';
ALTER TABLE sessions ADD COLUMN ua_hash TEXT DEFAULT '';

-- Invitation code hardening: store hashed codes (128-bit entropy)
ALTER TABLE workspace_invitations ADD COLUMN code_hash TEXT DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_invitations_code_hash ON workspace_invitations(code_hash);
