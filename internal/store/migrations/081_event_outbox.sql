-- Transactional event outbox (SPEC-3 §"The choke point, with an outbox",
-- PLAN-2656 phase 0 / TASK-2658).
--
-- Before this table, every event emission happened in the HTTP handler AFTER
-- the store call returned — outside any transaction. That shape has two
-- failure modes and both are silent: a mutation that commits and then loses
-- its process emits nothing (the event is gone, and nothing records that it
-- should have existed), and a handler that emits before a later step fails
-- can leak an event for a mutation that never committed. Writing the event
-- into this table inside the SAME transaction as the mutation makes both
-- impossible: the event and the row it describes commit or roll back
-- together. Everything downstream — webhooks, SSE, and later the binding
-- engine — drains from here, which is what lets SPEC-3's delivery guarantees
-- be stated honestly rather than hoped for.
--
-- DELIBERATELY NO FOREIGN KEYS on workspace_id / subject_id, which is the one
-- surprising thing in this schema. An outbox row must outlive its subject:
-- the whole point of an item.deleted event is that it is dispatched after the
-- item is gone, and SPEC-3 §Bindings requires deleted items stay addressable
-- through the payload snapshot. An ON DELETE CASCADE would delete exactly the
-- events that matter most, and a RESTRICT would make purge fail on any
-- workspace with undrained events. Orphan rows are handled by retention
-- (drained rows are prunable), not by referential integrity.
CREATE TABLE IF NOT EXISTS event_outbox (
    -- The public event id. Consumers dedupe on it — SPEC-3 §Delivery
    -- guarantees promises webhooks at-least-once with duplicates possible by
    -- design, and this is the value that makes that promise usable.
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL,

    -- Canonical events/1 name (item.created, item.status_changed, ...). The
    -- taxonomy is closed: a mutation whose name is not in events/1 writes no
    -- row at all, so silence about a mutation type is a versioned fact rather
    -- than a missing INSERT.
    event_type    TEXT NOT NULL,

    -- What the event is about. subject_id is nullable because not every
    -- canonical event has a row-shaped subject (member.joined, pack.*).
    subject_kind  TEXT NOT NULL,
    subject_id    TEXT,

    -- The event payload: the subject snapshot plus envelope pseudo-fields
    -- (prior_status for status_changed, per SPEC-3 §Bindings). Predicates
    -- evaluate against THIS, never against the live store, which is what
    -- makes binding evaluation deterministic regardless of dispatch delay.
    payload       TEXT NOT NULL,

    -- Cost discipline (SPEC-3 §L5): binding-triggered mutations inherit
    -- hop+1 and the kernel drops past depth 4. Bounds synchronous cascades
    -- only — async re-entry (a webhook consumer calling back through the API)
    -- legitimately starts fresh at 0.
    hop           INTEGER NOT NULL DEFAULT 0,

    -- The event's OWN timestamp, not dispatch time. SPEC-3 §Bindings pins
    -- time-relative predicates (`within`) to this value so a delayed drain
    -- cannot change how a predicate evaluates.
    occurred_at   TEXT NOT NULL,

    -- Drain bookkeeping. NULL dispatched_at is the pending set.
    dispatched_at TEXT,
    attempts      INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT
);

-- The drain query's index: pending rows in event order. Partial so it stays
-- small — the pending set is transient while the table as a whole retains
-- dispatched history until pruned.
--
-- Ordering is (occurred_at, id) and v1 does NOT promise event ordering to
-- consumers. Timestamps can tie, so id breaks ties for determinism of the
-- drain itself, not as an ordering contract. Anything needing true ordering
-- has to earn a monotonic sequence in a later contract version rather than
-- read one into this index.
CREATE INDEX IF NOT EXISTS idx_event_outbox_pending
    ON event_outbox(occurred_at, id)
    WHERE dispatched_at IS NULL;

-- Retention/pruning scans dispatched rows oldest-first.
CREATE INDEX IF NOT EXISTS idx_event_outbox_dispatched
    ON event_outbox(dispatched_at)
    WHERE dispatched_at IS NOT NULL;

-- Workspace-scoped reads (inspection, per-workspace quota accounting under
-- SPEC-3 §L5).
CREATE INDEX IF NOT EXISTS idx_event_outbox_workspace
    ON event_outbox(workspace_id, occurred_at);
