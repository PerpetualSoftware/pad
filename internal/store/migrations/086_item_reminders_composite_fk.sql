-- IDEA-2883: make the item/workspace agreement a SCHEMA CONSTRAINT rather
-- than a predicate repeated at every read.
--
-- item_reminders.workspace_id is denormalized from the item (see 085 for why:
-- the scheduler tick claims and scopes work without joining items on every
-- pass). Denormalization creates a value that CAN disagree with its source,
-- and IDEA-2641 closed that class one read at a time across codex rounds 13
-- and 14 — reminderFireable, the Postgres pin, ListPendingReminders, the
-- export query, then GetReminder and ListRemindersForItem. Five sites now
-- spell the same identity. The sixth someone adds will not, and the round
-- that finds it will be the sixteenth.
--
-- A composite foreign key makes the disagreeing row unrepresentable. The
-- read-side predicates STAY — they are also what makes the tests express the
-- invariant — but they become belt on top of braces rather than the only
-- thing standing between a denormalized column and its source.
--
-- NO ON UPDATE CASCADE, deliberately. items.workspace_id is never updated:
-- enumerated across internal/ and cmd/ (no `UPDATE items SET workspace_id`
-- anywhere, and UpdateItem's dynamic set list has no such column), and the
-- cross-workspace "move" is copy-plus-archive against a NEW item row
-- (items_cross_workspace_copy.go, PLAN-2357), so no row migrates between
-- workspaces. If a future feature does move one, this constraint should
-- REFUSE it rather than quietly rewrite the reminders underneath it — moving
-- an item's reminders is a decision that feature has to make on purpose.
--
-- SQLite cannot ADD a constraint, so the table is rebuilt via the standard
-- recipe, following migration 055. The runner supports this directly:
-- applySQLiteMigration pins the migration to ONE connection, treats
-- `PRAGMA foreign_keys = OFF` as a before-transaction pragma, restores it on
-- a defer covering every return path, and wraps the body plus its
-- schema_migrations row in a single transaction (store.go, IDEA-1485).
--
-- 084_nul_invariant_triggers.sql does not cover item_reminders, so the
-- rebuild has three indexes to recreate and no triggers.

PRAGMA foreign_keys = OFF;

-- The parent key. A composite FK needs the referenced columns to carry a
-- UNIQUE index; items(id) is already the primary key, so this index is
-- redundant for uniqueness and exists solely to be referenceable. Measured on
-- both drivers before writing this migration: SQLite (modernc, foreign_keys
-- on) and Postgres both accept a plain unique INDEX as an FK target, refuse
-- the disagreeing child row, and cascade through the composite key.
CREATE UNIQUE INDEX IF NOT EXISTS idx_items_id_workspace ON items(id, workspace_id);

DROP TABLE IF EXISTS item_reminders_new;

CREATE TABLE item_reminders_new (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    item_id      TEXT NOT NULL,
    remind_at    TEXT NOT NULL,
    fired_at     TEXT,
    acked_at     TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,

    -- Replaces 085's single-column `item_id REFERENCES items(id)`. The
    -- composite form implies it — item_id still has to name a live row — and
    -- keeps ON DELETE CASCADE, which was verified to fire through the
    -- composite key rather than being silently dropped with the old FK.
    FOREIGN KEY (item_id, workspace_id) REFERENCES items(id, workspace_id) ON DELETE CASCADE
);

-- The copy is JOIN-FILTERED, which is the repair pass. A row that disagrees
-- with its item — or whose item no longer exists at all — cannot be inserted
-- into the new table, so it has to be resolved here rather than failing the
-- migration on live data (055's precedent: backfill before constraining).
--
-- DROPPED, not repaired, and that is a choice rather than the easy path. A
-- disagreeing row is already unreachable: all five read predicates filter it,
-- so it can never fire, never be listed, and never be acknowledged. Repairing
-- it to the item's workspace would RESURRECT it — an upgrade would deliver a
-- notification for an instant long past, which is the one outcome nobody
-- asked for. Deleting it removes nothing any surface could ever show.
--
-- Rows are not expected to exist: CreateReminder derives workspace_id from
-- the item itself, and the import path writes an id minted in the same
-- workspace. That is an argument about the write doors, not a census, which
-- is exactly why this filter is here instead of a bare INSERT ... SELECT *.
INSERT INTO item_reminders_new (id, workspace_id, item_id, remind_at, fired_at, acked_at, created_at, updated_at)
SELECT r.id, r.workspace_id, r.item_id, r.remind_at, r.fired_at, r.acked_at, r.created_at, r.updated_at
FROM item_reminders r
JOIN items i ON i.id = r.item_id AND i.workspace_id = r.workspace_id;

DROP TABLE item_reminders;

ALTER TABLE item_reminders_new RENAME TO item_reminders;

-- The three indexes from 085, recreated verbatim. Their comments live there.
CREATE INDEX IF NOT EXISTS idx_item_reminders_armed
    ON item_reminders(remind_at)
    WHERE fired_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_item_reminders_unacked
    ON item_reminders(workspace_id, fired_at)
    WHERE fired_at IS NOT NULL AND acked_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_item_reminders_item
    ON item_reminders(item_id, remind_at);

PRAGMA foreign_keys = ON;
