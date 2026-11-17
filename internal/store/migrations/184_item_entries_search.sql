-- TASK-4491: full-text search over implementation notes and decision logs.
--
-- item_entries holds one row per implementation note or decision-log entry
-- found in items.fields, so a search hit can name the entry it matched by its
-- stored id. It is DERIVED: triggers on items keep it in step with every
-- writer of items.fields, and nothing else writes it. item_entries_fts is an
-- external-content FTS5 index over it, kept by the same trigger pattern as
-- comments_fts (007).
--
-- kind is note or decision. head is the summary or decision, body the details
-- or rationale. entry_index is the 0-based position in the array, entry_id the
-- stored id (empty when the entry has none yet). A member that is not a
-- string is indexed as the empty string, an element that is not an object is
-- skipped, and fields that are not valid JSON index nothing, so no write of
-- items.fields can fail here. Every insert is INSERT OR IGNORE for the same
-- reason: the index never refuses an item write. For the same reason a value
-- is cut at its first NUL character. items.fields refuses a NUL itself (084),
-- but a legacy row can still hold one, and the NUL triggers on these columns
-- (185) would otherwise refuse that row on its next write. It is a cut, not a
-- removal, because replace() treats a NUL pattern as empty and returns its
-- input unchanged.
--
-- The extraction is inlined in each trigger rather than shared through a VIEW,
-- because a view over items would break the ALTER TABLE ... RENAME step of a
-- future items-table rebuild. Nested iif keeps json_type and json_each away
-- from a malformed blob: iif is a CASE (SQLite compiles it to the same
-- bytecode) and so evaluates lazily, which AND is not guaranteed to do. It is
-- written as iif rather than CASE because the migration runner splits
-- statements on BEGIN and END (findStatementEnd in store.go), and the END of a
-- CASE inside a trigger body would end the trigger early.
--
-- FUTURE ITEMS-TABLE REBUILD: a migration that rebuilds items (create new,
-- copy, drop, rename) drops the three item_entries_items_* triggers with the
-- old table, and must recreate them from this file. expectedFTSTriggers in
-- store.go lists them, so startup warns if they are missing.
--
-- RECOVERY. A drifted index is rebuilt from items without touching user data.
-- Run, in one transaction:
--
--   DELETE FROM item_entries;
--   then both backfill INSERT OR IGNORE statements at the end of this file,
--   verbatim;
--   then INSERT INTO item_entries_fts(item_entries_fts) VALUES (R);
--
-- where R is the word rebuild as a single-quoted string literal: the FTS5
-- rebuild command, the statement 095 ends with for items_fts. The DELETE and
-- the backfill re-derive every row of item_entries from items.fields; the
-- rebuild then regenerates item_entries_fts from item_entries, so it is right
-- even if the FTS index had drifted on its own. Only item_entries and
-- item_entries_fts are written.
--
-- Cost: the backfill parses every items.fields once per kind and tokenizes
-- every entry. It runs at startup, before the server listens, and is named in
-- slowSQLiteMigrations so the log says why a first boot is slow.

CREATE TABLE IF NOT EXISTS item_entries (
    id          INTEGER PRIMARY KEY,
    item_id     TEXT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('note', 'decision')),
    entry_index INTEGER NOT NULL,
    entry_id    TEXT NOT NULL,
    head        TEXT NOT NULL,
    body        TEXT NOT NULL,
    UNIQUE (item_id, kind, entry_index)
);

CREATE VIRTUAL TABLE IF NOT EXISTS item_entries_fts USING fts5(
    head,
    body,
    content='item_entries',
    content_rowid='id',
    tokenize='porter unicode61'
);

DROP TRIGGER IF EXISTS item_entries_fts_insert;
DROP TRIGGER IF EXISTS item_entries_fts_update;
DROP TRIGGER IF EXISTS item_entries_fts_delete;

CREATE TRIGGER item_entries_fts_insert AFTER INSERT ON item_entries BEGIN
    INSERT INTO item_entries_fts(rowid, head, body) VALUES (NEW.id, NEW.head, NEW.body);
END;

CREATE TRIGGER item_entries_fts_update AFTER UPDATE ON item_entries BEGIN
    INSERT INTO item_entries_fts(item_entries_fts, rowid, head, body) VALUES ('delete', OLD.id, OLD.head, OLD.body);
    INSERT INTO item_entries_fts(rowid, head, body) VALUES (NEW.id, NEW.head, NEW.body);
END;

CREATE TRIGGER item_entries_fts_delete AFTER DELETE ON item_entries BEGIN
    INSERT INTO item_entries_fts(item_entries_fts, rowid, head, body) VALUES ('delete', OLD.id, OLD.head, OLD.body);
END;

DROP TRIGGER IF EXISTS item_entries_items_insert;
DROP TRIGGER IF EXISTS item_entries_items_update;
DROP TRIGGER IF EXISTS item_entries_items_delete;

-- A new item: index both logs.
CREATE TRIGGER item_entries_items_insert AFTER INSERT ON items BEGIN
    INSERT OR IGNORE INTO item_entries (item_id, kind, entry_index, entry_id, head, body)
    SELECT NEW.id, 'note', je.key, iif(json_type(je.value, '$.id') = 'text', iif(instr(json_extract(je.value, '$.id'), char(0)) > 0, substr(json_extract(je.value, '$.id'), 1, instr(json_extract(je.value, '$.id'), char(0)) - 1), json_extract(je.value, '$.id')), ''), iif(json_type(je.value, '$.summary') = 'text', iif(instr(json_extract(je.value, '$.summary'), char(0)) > 0, substr(json_extract(je.value, '$.summary'), 1, instr(json_extract(je.value, '$.summary'), char(0)) - 1), json_extract(je.value, '$.summary')), ''), iif(json_type(je.value, '$.details') = 'text', iif(instr(json_extract(je.value, '$.details'), char(0)) > 0, substr(json_extract(je.value, '$.details'), 1, instr(json_extract(je.value, '$.details'), char(0)) - 1), json_extract(je.value, '$.details')), '')
    FROM json_each(iif(json_valid(NEW.fields), iif(json_type(NEW.fields, '$.implementation_notes') = 'array', NEW.fields, '{"implementation_notes":[]}'), '{"implementation_notes":[]}'), '$.implementation_notes') je
    WHERE je.type = 'object';
    INSERT OR IGNORE INTO item_entries (item_id, kind, entry_index, entry_id, head, body)
    SELECT NEW.id, 'decision', je.key, iif(json_type(je.value, '$.id') = 'text', iif(instr(json_extract(je.value, '$.id'), char(0)) > 0, substr(json_extract(je.value, '$.id'), 1, instr(json_extract(je.value, '$.id'), char(0)) - 1), json_extract(je.value, '$.id')), ''), iif(json_type(je.value, '$.decision') = 'text', iif(instr(json_extract(je.value, '$.decision'), char(0)) > 0, substr(json_extract(je.value, '$.decision'), 1, instr(json_extract(je.value, '$.decision'), char(0)) - 1), json_extract(je.value, '$.decision')), ''), iif(json_type(je.value, '$.rationale') = 'text', iif(instr(json_extract(je.value, '$.rationale'), char(0)) > 0, substr(json_extract(je.value, '$.rationale'), 1, instr(json_extract(je.value, '$.rationale'), char(0)) - 1), json_extract(je.value, '$.rationale')), '')
    FROM json_each(iif(json_valid(NEW.fields), iif(json_type(NEW.fields, '$.decision_log') = 'array', NEW.fields, '{"decision_log":[]}'), '{"decision_log":[]}'), '$.decision_log') je
    WHERE je.type = 'object';
END;

-- A fields write that changes either log. Per kind: delete the rows whose
-- position no longer holds the same entry, then insert the positions that have
-- no row. An append therefore inserts one row and rewrites none, and a write
-- that leaves both arrays alone is skipped by the WHEN clause.
CREATE TRIGGER item_entries_items_update AFTER UPDATE OF fields ON items
WHEN iif(json_valid(OLD.fields) AND json_valid(NEW.fields),
         json_extract(OLD.fields, '$.implementation_notes') IS NOT json_extract(NEW.fields, '$.implementation_notes')
         OR json_extract(OLD.fields, '$.decision_log') IS NOT json_extract(NEW.fields, '$.decision_log'),
         1)
BEGIN
    DELETE FROM item_entries WHERE item_id = NEW.id AND kind = 'note' AND id NOT IN (
        SELECT e.id
        FROM json_each(iif(json_valid(NEW.fields), iif(json_type(NEW.fields, '$.implementation_notes') = 'array', NEW.fields, '{"implementation_notes":[]}'), '{"implementation_notes":[]}'), '$.implementation_notes') je
        JOIN item_entries e ON e.item_id = NEW.id AND e.kind = 'note' AND e.entry_index = je.key
        WHERE je.type = 'object'
          AND e.entry_id = iif(json_type(je.value, '$.id') = 'text', iif(instr(json_extract(je.value, '$.id'), char(0)) > 0, substr(json_extract(je.value, '$.id'), 1, instr(json_extract(je.value, '$.id'), char(0)) - 1), json_extract(je.value, '$.id')), '')
          AND e.head = iif(json_type(je.value, '$.summary') = 'text', iif(instr(json_extract(je.value, '$.summary'), char(0)) > 0, substr(json_extract(je.value, '$.summary'), 1, instr(json_extract(je.value, '$.summary'), char(0)) - 1), json_extract(je.value, '$.summary')), '')
          AND e.body = iif(json_type(je.value, '$.details') = 'text', iif(instr(json_extract(je.value, '$.details'), char(0)) > 0, substr(json_extract(je.value, '$.details'), 1, instr(json_extract(je.value, '$.details'), char(0)) - 1), json_extract(je.value, '$.details')), '')
    );
    INSERT OR IGNORE INTO item_entries (item_id, kind, entry_index, entry_id, head, body)
    SELECT NEW.id, 'note', je.key, iif(json_type(je.value, '$.id') = 'text', iif(instr(json_extract(je.value, '$.id'), char(0)) > 0, substr(json_extract(je.value, '$.id'), 1, instr(json_extract(je.value, '$.id'), char(0)) - 1), json_extract(je.value, '$.id')), ''), iif(json_type(je.value, '$.summary') = 'text', iif(instr(json_extract(je.value, '$.summary'), char(0)) > 0, substr(json_extract(je.value, '$.summary'), 1, instr(json_extract(je.value, '$.summary'), char(0)) - 1), json_extract(je.value, '$.summary')), ''), iif(json_type(je.value, '$.details') = 'text', iif(instr(json_extract(je.value, '$.details'), char(0)) > 0, substr(json_extract(je.value, '$.details'), 1, instr(json_extract(je.value, '$.details'), char(0)) - 1), json_extract(je.value, '$.details')), '')
    FROM json_each(iif(json_valid(NEW.fields), iif(json_type(NEW.fields, '$.implementation_notes') = 'array', NEW.fields, '{"implementation_notes":[]}'), '{"implementation_notes":[]}'), '$.implementation_notes') je
    WHERE je.type = 'object'
      AND NOT EXISTS (SELECT 1 FROM item_entries e WHERE e.item_id = NEW.id AND e.kind = 'note' AND e.entry_index = je.key);
    DELETE FROM item_entries WHERE item_id = NEW.id AND kind = 'decision' AND id NOT IN (
        SELECT e.id
        FROM json_each(iif(json_valid(NEW.fields), iif(json_type(NEW.fields, '$.decision_log') = 'array', NEW.fields, '{"decision_log":[]}'), '{"decision_log":[]}'), '$.decision_log') je
        JOIN item_entries e ON e.item_id = NEW.id AND e.kind = 'decision' AND e.entry_index = je.key
        WHERE je.type = 'object'
          AND e.entry_id = iif(json_type(je.value, '$.id') = 'text', iif(instr(json_extract(je.value, '$.id'), char(0)) > 0, substr(json_extract(je.value, '$.id'), 1, instr(json_extract(je.value, '$.id'), char(0)) - 1), json_extract(je.value, '$.id')), '')
          AND e.head = iif(json_type(je.value, '$.decision') = 'text', iif(instr(json_extract(je.value, '$.decision'), char(0)) > 0, substr(json_extract(je.value, '$.decision'), 1, instr(json_extract(je.value, '$.decision'), char(0)) - 1), json_extract(je.value, '$.decision')), '')
          AND e.body = iif(json_type(je.value, '$.rationale') = 'text', iif(instr(json_extract(je.value, '$.rationale'), char(0)) > 0, substr(json_extract(je.value, '$.rationale'), 1, instr(json_extract(je.value, '$.rationale'), char(0)) - 1), json_extract(je.value, '$.rationale')), '')
    );
    INSERT OR IGNORE INTO item_entries (item_id, kind, entry_index, entry_id, head, body)
    SELECT NEW.id, 'decision', je.key, iif(json_type(je.value, '$.id') = 'text', iif(instr(json_extract(je.value, '$.id'), char(0)) > 0, substr(json_extract(je.value, '$.id'), 1, instr(json_extract(je.value, '$.id'), char(0)) - 1), json_extract(je.value, '$.id')), ''), iif(json_type(je.value, '$.decision') = 'text', iif(instr(json_extract(je.value, '$.decision'), char(0)) > 0, substr(json_extract(je.value, '$.decision'), 1, instr(json_extract(je.value, '$.decision'), char(0)) - 1), json_extract(je.value, '$.decision')), ''), iif(json_type(je.value, '$.rationale') = 'text', iif(instr(json_extract(je.value, '$.rationale'), char(0)) > 0, substr(json_extract(je.value, '$.rationale'), 1, instr(json_extract(je.value, '$.rationale'), char(0)) - 1), json_extract(je.value, '$.rationale')), '')
    FROM json_each(iif(json_valid(NEW.fields), iif(json_type(NEW.fields, '$.decision_log') = 'array', NEW.fields, '{"decision_log":[]}'), '{"decision_log":[]}'), '$.decision_log') je
    WHERE je.type = 'object'
      AND NOT EXISTS (SELECT 1 FROM item_entries e WHERE e.item_id = NEW.id AND e.kind = 'decision' AND e.entry_index = je.key);
END;

-- Hard delete (the workspace purge) removes the rows. Soft delete and
-- restore only change deleted_at, so the rows stay and search filters them.
CREATE TRIGGER item_entries_items_delete AFTER DELETE ON items BEGIN
    DELETE FROM item_entries WHERE item_id = OLD.id;
END;

-- Backfill, once per kind. Also the recovery statements named above.
INSERT OR IGNORE INTO item_entries (item_id, kind, entry_index, entry_id, head, body)
SELECT i.id, 'note', je.key, iif(json_type(je.value, '$.id') = 'text', iif(instr(json_extract(je.value, '$.id'), char(0)) > 0, substr(json_extract(je.value, '$.id'), 1, instr(json_extract(je.value, '$.id'), char(0)) - 1), json_extract(je.value, '$.id')), ''), iif(json_type(je.value, '$.summary') = 'text', iif(instr(json_extract(je.value, '$.summary'), char(0)) > 0, substr(json_extract(je.value, '$.summary'), 1, instr(json_extract(je.value, '$.summary'), char(0)) - 1), json_extract(je.value, '$.summary')), ''), iif(json_type(je.value, '$.details') = 'text', iif(instr(json_extract(je.value, '$.details'), char(0)) > 0, substr(json_extract(je.value, '$.details'), 1, instr(json_extract(je.value, '$.details'), char(0)) - 1), json_extract(je.value, '$.details')), '')
FROM items i, json_each(iif(json_valid(i.fields), iif(json_type(i.fields, '$.implementation_notes') = 'array', i.fields, '{"implementation_notes":[]}'), '{"implementation_notes":[]}'), '$.implementation_notes') je
WHERE je.type = 'object';

INSERT OR IGNORE INTO item_entries (item_id, kind, entry_index, entry_id, head, body)
SELECT i.id, 'decision', je.key, iif(json_type(je.value, '$.id') = 'text', iif(instr(json_extract(je.value, '$.id'), char(0)) > 0, substr(json_extract(je.value, '$.id'), 1, instr(json_extract(je.value, '$.id'), char(0)) - 1), json_extract(je.value, '$.id')), ''), iif(json_type(je.value, '$.decision') = 'text', iif(instr(json_extract(je.value, '$.decision'), char(0)) > 0, substr(json_extract(je.value, '$.decision'), 1, instr(json_extract(je.value, '$.decision'), char(0)) - 1), json_extract(je.value, '$.decision')), ''), iif(json_type(je.value, '$.rationale') = 'text', iif(instr(json_extract(je.value, '$.rationale'), char(0)) > 0, substr(json_extract(je.value, '$.rationale'), 1, instr(json_extract(je.value, '$.rationale'), char(0)) - 1), json_extract(je.value, '$.rationale')), '')
FROM items i, json_each(iif(json_valid(i.fields), iif(json_type(i.fields, '$.decision_log') = 'array', i.fields, '{"decision_log":[]}'), '{"decision_log":[]}'), '$.decision_log') je
WHERE je.type = 'object';
