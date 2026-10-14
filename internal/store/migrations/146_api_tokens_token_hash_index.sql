-- Migration 146: index api_tokens.token_hash (BUG-3668).
--
-- Every token-authenticated request (REST and MCP) looks its token up by
-- token_hash, and nothing indexed it, so each lookup scanned the table; an
-- absent hash, which every well-formed invalid token is, had to scan all of
-- it. A plain index, as sessions, password resets and email verification
-- tokens have: a UNIQUE one would fail this migration on a database that
-- already held a duplicate, and the lookup does not depend on uniqueness.
CREATE INDEX IF NOT EXISTS idx_api_tokens_token_hash ON api_tokens(token_hash);
