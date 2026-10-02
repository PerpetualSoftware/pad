-- Migration 105: record how a session was issued (BUG-3350). A session minted
-- for the CLI (browser approval, or `pad auth login -i`) is a bearer
-- credential and must never be accepted as a browser cookie: sent as one, it
-- picked up the cookie-only platform-admin bypass that bearer use is denied
-- (BUG-1616). SessionAuth now accepts a cookie only for kind = 'web'.
--
-- Backfill from device_info: only the browser-approval flow ever recorded
-- 'cli-browser-auth'. Password logins from the CLI were recorded as 'web'
-- and stay so until they expire (lead's ruling: no forced re-login; the CLI
-- now marks its logins, so new ones are 'cli').
ALTER TABLE sessions ADD COLUMN kind TEXT NOT NULL DEFAULT 'web';
UPDATE sessions SET kind = 'cli' WHERE device_info = 'cli-browser-auth';
