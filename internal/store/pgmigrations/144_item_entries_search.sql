SET LOCAL lock_timeout = '5s';
-- TASK-4491: full-text search over implementation notes and decision logs.
--
-- item_entries holds one row per implementation note or decision-log entry
-- found in items.fields, so a search hit can name the entry it matched by its
-- stored id. It is DERIVED: triggers on items keep it in step with every
-- writer of items.fields, and nothing else writes it. The SQLite twin is
-- migrations/184, which uses an FTS5 table where this uses search_vector.
--
-- kind is note or decision. head is the summary or decision, body the details
-- or rationale. entry_index is the 0-based position in the array (ordinality
-- minus one, so it matches the SQLite json_each key), entry_id the stored id
-- (empty when the entry has none yet). A member that is not a string is
-- indexed as the empty string and an element that is not an object is
-- skipped, so no write of items.fields can fail here. Every insert is ON
-- CONFLICT DO NOTHING for the same reason: the index never refuses an item
-- write.
--
-- TSVECTOR CAP. A tsvector is limited to 1 MB, and to_tsvector raises (string
-- is too long for tsvector) past it. An item write accepts a 2 MiB JSON body
-- (defaultJSONBodyLimit, internal/server/server.go), so one note can carry
-- about 2 MiB of distinct words, which is past the limit. The search_vector
-- trigger therefore indexes only the first 65536 characters of head and of
-- body. Worst case, one distinct one-character word per two input characters,
-- a lexeme costs about 12 bytes (4 entry, up to 4 text, 2 count, 2 position),
-- so 2 x 65536 characters stay under 800 KB. The stored head and body are not
-- cut; ts_headline still reads them whole. Text past the cap does not match.
--
-- Locking: the triggers are created before the backfill and the whole file is
-- one transaction, so CREATE TRIGGER holds its lock on items until COMMIT and
-- no write can land between the backfill and the triggers. The backfill is a
-- single statement so its cost is one measurement. Re-runnable: IF NOT EXISTS,
-- CREATE OR REPLACE, DROP TRIGGER IF EXISTS, and a conflict-safe backfill.
--
-- RECOVERY. A drifted index is rebuilt from items without touching user data.
-- Run, in one transaction:
--
--   TRUNCATE item_entries;
--   INSERT INTO item_entries (item_id, kind, entry_index, entry_id, head, body)
--   SELECT i.id, s.kind, s.entry_index, s.entry_id, s.head, s.body
--   FROM items i, LATERAL item_entries_source(i.fields) s
--   ON CONFLICT (item_id, kind, entry_index) DO NOTHING;
--
-- The second statement is the backfill at the end of this file, verbatim; the
-- BEFORE INSERT trigger recomputes every search_vector as the rows land. Only
-- item_entries is written.
--
-- FUTURE ITEMS-TABLE REBUILD: a migration that recreates items must recreate
-- items_entries_insert and items_entries_update from this file, and the
-- item_entries foreign key (ON DELETE CASCADE, which is how a hard delete
-- removes the rows here).

CREATE TABLE IF NOT EXISTS item_entries (
    id            BIGSERIAL PRIMARY KEY,
    item_id       TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL CHECK (kind IN ('note', 'decision')),
    entry_index   INTEGER NOT NULL,
    entry_id      TEXT NOT NULL,
    head          TEXT NOT NULL,
    body          TEXT NOT NULL,
    search_vector TSVECTOR,
    UNIQUE (item_id, kind, entry_index)
);

CREATE INDEX IF NOT EXISTS idx_item_entries_fts ON item_entries USING GIN(search_vector);

CREATE OR REPLACE FUNCTION item_entries_search_vector_update() RETURNS TRIGGER AS $$
BEGIN
    NEW.search_vector :=
        setweight(to_tsvector('english', left(NEW.head, 65536)), 'A') ||
        setweight(to_tsvector('english', left(NEW.body, 65536)), 'B');
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS item_entries_search_vector_trigger ON item_entries;
CREATE TRIGGER item_entries_search_vector_trigger
    BEFORE INSERT OR UPDATE OF head, body ON item_entries
    FOR EACH ROW EXECUTE FUNCTION item_entries_search_vector_update();

-- The entries of one fields blob, both kinds. The CASE keeps
-- jsonb_array_elements away from a value that is not an array.
CREATE OR REPLACE FUNCTION item_entries_source(f JSONB)
RETURNS TABLE (kind TEXT, entry_index INTEGER, entry_id TEXT, head TEXT, body TEXT) AS $$
    SELECT 'note'::text, (je.ord - 1)::integer,
           CASE WHEN jsonb_typeof(je.value -> 'id') = 'string' THEN je.value ->> 'id' ELSE '' END,
           CASE WHEN jsonb_typeof(je.value -> 'summary') = 'string' THEN je.value ->> 'summary' ELSE '' END,
           CASE WHEN jsonb_typeof(je.value -> 'details') = 'string' THEN je.value ->> 'details' ELSE '' END
    FROM jsonb_array_elements(CASE WHEN jsonb_typeof(f -> 'implementation_notes') = 'array' THEN f -> 'implementation_notes' ELSE '[]'::jsonb END)
         WITH ORDINALITY AS je(value, ord)
    WHERE jsonb_typeof(je.value) = 'object'
    UNION ALL
    SELECT 'decision'::text, (je.ord - 1)::integer,
           CASE WHEN jsonb_typeof(je.value -> 'id') = 'string' THEN je.value ->> 'id' ELSE '' END,
           CASE WHEN jsonb_typeof(je.value -> 'decision') = 'string' THEN je.value ->> 'decision' ELSE '' END,
           CASE WHEN jsonb_typeof(je.value -> 'rationale') = 'string' THEN je.value ->> 'rationale' ELSE '' END
    FROM jsonb_array_elements(CASE WHEN jsonb_typeof(f -> 'decision_log') = 'array' THEN f -> 'decision_log' ELSE '[]'::jsonb END)
         WITH ORDINALITY AS je(value, ord)
    WHERE jsonb_typeof(je.value) = 'object'
$$ LANGUAGE sql IMMUTABLE;

-- Bring one item into step: delete the rows whose position no longer holds
-- the same entry, then insert the positions that have no row. An append
-- inserts one row and rewrites none.
CREATE OR REPLACE FUNCTION item_entries_sync(p_item_id TEXT, f JSONB) RETURNS void AS $$
BEGIN
    WITH s AS MATERIALIZED (SELECT * FROM item_entries_source(f))
    DELETE FROM item_entries e
    WHERE e.item_id = p_item_id
      AND NOT EXISTS (
          SELECT 1 FROM s
          WHERE s.kind = e.kind AND s.entry_index = e.entry_index
            AND s.entry_id = e.entry_id AND s.head = e.head AND s.body = e.body
      );
    INSERT INTO item_entries (item_id, kind, entry_index, entry_id, head, body)
    SELECT p_item_id, s.kind, s.entry_index, s.entry_id, s.head, s.body
    FROM item_entries_source(f) s
    ON CONFLICT (item_id, kind, entry_index) DO NOTHING;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION items_entries_sync_trigger() RETURNS TRIGGER AS $$
BEGIN
    PERFORM item_entries_sync(NEW.id, NEW.fields);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS items_entries_insert ON items;
CREATE TRIGGER items_entries_insert
    AFTER INSERT ON items
    FOR EACH ROW EXECUTE FUNCTION items_entries_sync_trigger();

-- A write that leaves both arrays alone (a status patch, a seq bump) is
-- skipped here.
DROP TRIGGER IF EXISTS items_entries_update ON items;
CREATE TRIGGER items_entries_update
    AFTER UPDATE OF fields ON items
    FOR EACH ROW
    WHEN (OLD.fields -> 'implementation_notes' IS DISTINCT FROM NEW.fields -> 'implementation_notes'
       OR OLD.fields -> 'decision_log' IS DISTINCT FROM NEW.fields -> 'decision_log')
    EXECUTE FUNCTION items_entries_sync_trigger();

-- Backfill. Also the recovery statement named above.
INSERT INTO item_entries (item_id, kind, entry_index, entry_id, head, body)
SELECT i.id, s.kind, s.entry_index, s.entry_id, s.head, s.body
FROM items i, LATERAL item_entries_source(i.fields) s
ON CONFLICT (item_id, kind, entry_index) DO NOTHING;
