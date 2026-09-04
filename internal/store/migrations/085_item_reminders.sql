-- Item reminders: the fire-at-a-time primitive (IDEA-2641, GitHub #1010).
--
-- The gap this closes is that Pad's date handling was entirely REACTIVE. A
-- `due_date` field makes an item show up as overdue once someone asks the
-- dashboard, but nothing in the server ever ACTS at a target time, so
-- "revisit TASK-X on 2026-08-01" had to live in an external cron. This table
-- is the state a scheduler tick reads.
--
-- WHY A TABLE AND NOT AN ANNOTATION ON THE SCHEMA FIELD, which is the shape
-- the design sketch proposed and recon overturned: an annotation stored as a
-- new key on models.FieldDef does not survive an ordinary collection edit.
-- The web editor destructures each field into an EditableField and rebuilds a
-- fresh definition key-by-key on save (EditCollectionModal.svelte), so any key
-- it does not know about is dropped — `pattern` and `unique_scope` survive
-- only because two lines were hand-added for them. Independently,
-- models.CollectionSchema has fixed fields and no catch-all, so any Go
-- unmarshal+marshal round-trip strips unknown properties; that is the hazard
-- retargetRelationFieldsTx mutates raw JSON to avoid, and it names it in its
-- own comment. Both failures are SILENT and both take out a whole
-- collection's reminders at once. It is the same defect class that moved
-- traits out of the schema column in TASK-2657.
--
-- The table also gives the two semantics the annotation shape would have had
-- to invent somewhere a natural home: a reminder has a LIFECYCLE (armed,
-- fired, acknowledged, re-armed) and that lifecycle is per-reminder state, not
-- a property of a field definition.
--
-- SCOPE: one-shot reminders only. Recurrence multiplies the re-arm semantics
-- and is a separate item, not effort deferred.
--
-- WHAT THIS TABLE IS NOT: it is not where `due_date` lives. Due dates stay
-- ordinary schema date fields and keep their existing reactive behaviour;
-- `due` (a state surfaces react to) and `remind_at` (an instant the server
-- acts on) are different primitives and this is only the second one.
CREATE TABLE IF NOT EXISTS item_reminders (
    id           TEXT PRIMARY KEY,

    -- Denormalized from the item so the scheduler tick can claim and scope
    -- work without joining items on every pass, and so a workspace-scoped
    -- read stays a single-table query.
    workspace_id TEXT NOT NULL,

    -- ON DELETE CASCADE, unlike event_outbox's deliberate absence of foreign
    -- keys. An outbox row must outlive its subject because an item.deleted
    -- event is dispatched after the item is gone. A reminder is the opposite:
    -- it is an instruction to say something about an item LATER, and once the
    -- item is gone there is nothing to say. Firing a reminder for a deleted
    -- item would be a notification a user can do nothing with.
    item_id      TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,

    -- The instant to fire at: RFC3339, UTC, always. Deliberately NOT a
    -- `date`-typed schema value, which admits both YYYY-MM-DD and full
    -- RFC3339 (internal/items/validate.go) and is compared elsewhere against
    -- the SERVER'S LOCAL calendar day. A fire-at time cannot inherit that
    -- ambiguity: "2026-08-01" does not name an instant, and the difference
    -- between the two shapes is a whole day of drift. The remaining
    -- timezone question for due_date is tracked as its own item.
    remind_at    TEXT NOT NULL,

    -- Lifecycle. NULL fired_at is the ARMED set — the only rows a tick
    -- considers. Set once the tick has emitted the event.
    fired_at     TEXT,

    -- Explicit acknowledgement, and the reason it is a separate column rather
    -- than clearing fired_at: the three states (armed / fired-unacked /
    -- fired-acked) are distinguishable only if firing and acking are recorded
    -- separately. Clearing fired_at on ack would return the row to the armed
    -- set and fire it again on the next tick.
    --
    -- Acking is EXPLICIT and nothing else acks. In particular an item
    -- reaching a terminal status does not: that would make every status write
    -- a reminder mutation, and it would silently consume a reminder a user
    -- may have set precisely to fire after the work was finished. The poll
    -- surface instead FILTERS fired-unacked reminders whose item is terminal,
    -- without mutating the row — the user's intent stays in the table and the
    -- agent stops being shown a dead item.
    acked_at     TEXT,

    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

-- The tick's index: armed rows in fire order. Partial, so it holds only the
-- pending set rather than every reminder ever created — the armed set is
-- transient while the table retains fired rows as the record that a reminder
-- existed and went out.
CREATE INDEX IF NOT EXISTS idx_item_reminders_armed
    ON item_reminders(remind_at)
    WHERE fired_at IS NULL;

-- The poll surface's index: fired-but-unacked rows, which is what `pad
-- project next` / `ready` reads.
CREATE INDEX IF NOT EXISTS idx_item_reminders_unacked
    ON item_reminders(workspace_id, fired_at)
    WHERE fired_at IS NOT NULL AND acked_at IS NULL;

-- Per-item reads (an item's own reminders, and the cascade's lookup path).
CREATE INDEX IF NOT EXISTS idx_item_reminders_item
    ON item_reminders(item_id, remind_at);
