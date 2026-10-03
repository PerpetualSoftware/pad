-- Migration 114: a principal kind on users (SPEC-6 U4, TASK-3392).
--
-- An installed app acts as a bot user (DOC-3371 §3): a row in users, so it
-- rides every existing per-handler read check as an ordinary member. 'app'
-- marks that row. It is created only by CreateAppUserTx, with a password hash
-- that is not a bcrypt string and a reserved .invalid email, and every sign-in
-- door and credential mint refuses it. Every existing row is a person.
ALTER TABLE users ADD COLUMN kind TEXT NOT NULL DEFAULT 'human' CHECK (kind IN ('human', 'app'));
