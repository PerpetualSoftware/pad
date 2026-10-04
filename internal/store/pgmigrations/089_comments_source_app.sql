-- Migration 089: comments.source accepts 'app' (SPEC-6 U2b, TASK-3391).
-- Mirrors SQLite migration 115, which rebuilds the table; Postgres can swap
-- the constraint in place. The CHECK on comments.source was created unnamed
-- in 001_initial.sql, so it is found in the catalog by its definition, not
-- by an assumed default name. Exactly one such constraint must exist; the
-- block raises otherwise rather than leaving two or none.
DO $$
DECLARE
    found_name text;
    found_count int;
BEGIN
    SELECT count(*), min(con.conname)
      INTO found_count, found_name
      FROM pg_constraint con
      JOIN pg_class rel ON rel.oid = con.conrelid
      JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
     WHERE nsp.nspname = current_schema()
       AND rel.relname = 'comments'
       AND con.contype = 'c'
       AND pg_get_constraintdef(con.oid) ILIKE '%source%'
       AND pg_get_constraintdef(con.oid) ILIKE '%skill%';
    IF found_count <> 1 THEN
        RAISE EXCEPTION 'comments.source CHECK: expected exactly one, found %', found_count;
    END IF;
    EXECUTE format('ALTER TABLE comments DROP CONSTRAINT %I', found_name);
END $$;

ALTER TABLE comments ADD CONSTRAINT comments_source_check
    CHECK (source IN ('cli', 'web', 'skill', 'app'));
