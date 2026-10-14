-- Migration 116 (Postgres): index api_tokens.token_hash (BUG-3668).
-- Postgres counterpart to migrations/146_api_tokens_token_hash_index.sql; the rationale is there.
CREATE INDEX IF NOT EXISTS idx_api_tokens_token_hash ON api_tokens(token_hash);
