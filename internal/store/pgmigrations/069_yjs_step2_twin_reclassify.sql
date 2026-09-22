-- Migration 069: re-queue step2/update op-log rows for classification (BUG-3136).
-- Postgres twin of SQLite migration 092; see that file for the rationale.
UPDATE item_yjs_updates
   SET content_hash = NULL
 WHERE content_bearing
   AND substring(update_data from 1 for 2) IN ('\x0001'::bytea, '\x0002'::bytea);
