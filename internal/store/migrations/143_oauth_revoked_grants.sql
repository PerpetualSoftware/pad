-- BUG-3678: a revoked OAuth grant, by request_id. Revoking flips the grant's
-- token rows inactive, which a rotation also does, so the rows alone cannot
-- tell a revoked grant from a refreshed one, and a refresh that validated
-- before the revoke used to mint a fresh active pair after it. Issuance
-- refuses a request_id listed here (insertOAuthRequestRow), under the
-- subject's users row lock that every revoking door takes first.
CREATE TABLE IF NOT EXISTS oauth_revoked_grants (
    request_id TEXT PRIMARY KEY,
    revoked_at TEXT NOT NULL
);
