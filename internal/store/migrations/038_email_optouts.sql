-- Email opt-outs: tracks email addresses that have unsubscribed from non-transactional emails.
CREATE TABLE IF NOT EXISTS email_optouts (
    email      TEXT PRIMARY KEY,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
