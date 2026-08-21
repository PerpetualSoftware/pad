-- Migration 083: event_outbox claim columns (TASK-2714).
--
-- Pad Cloud runs N instances against one database, and every one of them runs
-- the drain. Without a claim, each pending row is delivered N times BY
-- CONSTRUCTION. SPEC-3 §Delivery guarantees permits duplicates — consumers
-- dedupe on the event id — but "occasionally, after a crash" and "always,
-- once per instance" are different promises, and only the first is one a
-- consumer can budget for.
--
-- The claim is a conditional UPDATE (see ClaimPendingOutboxEvents), the same
-- protocol BUG-2415 established for orphan GC. Candidate rows are SELECTed
-- first and then claimed one conditional UPDATE at a time; the select is only
-- discovery, and the predicate ON THE UPDATE is the arbiter, so two instances
-- racing on the same row produce one winner and one no-op rather than two
-- winners. (Codex round 1 flagged an earlier version of this comment for
-- describing it as a single statement doing both.) Dialect-uniform on purpose — the
-- alternative, Postgres FOR UPDATE SKIP LOCKED with a separate SQLite path,
-- is two implementations of one behaviour and only one of them ever runs in
-- the environment where it matters.
--
-- claimed_at doubles as the LEASE. A claimed row whose claim has aged past the
-- lease window is re-claimable: an instance that dies between claiming and
-- dispatching must not strand its rows, and at-least-once is exactly the
-- promise that makes re-claiming safe.
ALTER TABLE event_outbox ADD COLUMN claimed_at TEXT;
ALTER TABLE event_outbox ADD COLUMN claimed_by TEXT;

-- The claim query scans pending rows whose claim is absent or expired. The
-- existing pending index orders by (occurred_at, id); this one narrows to the
-- claim state so an instance does not walk rows another instance holds.
CREATE INDEX IF NOT EXISTS idx_event_outbox_claim
    ON event_outbox(claimed_at, occurred_at)
    WHERE dispatched_at IS NULL;
