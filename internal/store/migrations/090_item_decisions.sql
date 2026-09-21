-- Migration 090: typed-decision storage and its job queue (PLAN-3114 unit 2,
-- TASK-3117).
--
-- Two tables. item_decisions holds the provider's answers; decision_jobs is
-- the queue that says an item owes an evaluation. Both are keyed on the item
-- through the composite (item_id, workspace_id) foreign key migration 086
-- introduced, so a denormalized workspace_id cannot disagree with its item.
--
-- WHY A PRIVATE QUEUE AND NOT THE EVENT OUTBOX (TASK-3117 ruling 1). The
-- outbox's event names are the closed, PUBLIC events/1 contract that webhooks
-- deliver (internal/kernelevents), and the outbox has exactly one consumer:
-- one claimed_by, one dispatched_at, and a drain that acks every row at once
-- when no webhook dispatcher is configured. An internal job riding it would
-- have had to become a public event name, and would have been acked out from
-- under the runner on every instance with webhooks off. decision_jobs borrows
-- the outbox's DISCIPLINE instead: the job row is written in the same
-- transaction as the mutation that makes it owed, and claimed by lease.

-- WHY A STATE HASH AND NOT THE ITEM'S SEQ (TASK-3117 ruling 2). items.seq is
-- the wrong freshness key in both directions. A comment does not bump it
-- (store/comments.go writes no seq), so a comment-triggered evaluation keyed
-- on seq would find its own previous row and do nothing. And writes the
-- questions cannot see DO bump it — role-board reorders, wiki-link
-- re-resolution, collection-wide rewrites — so each would cost a provider
-- call. state_hash is sha256 of the exact bytes sent to the provider, which is
-- precisely "did anything the model reads change". item_seq is kept for the
-- audit: which version of the row the answer was computed against.
CREATE TABLE IF NOT EXISTS item_decisions (
    id              TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL,
    item_id         TEXT NOT NULL,

    -- The registered question set this answer came from, and the key of the
    -- question within it. Both are server-registered names, never caller text.
    question_set    TEXT NOT NULL,
    question_key    TEXT NOT NULL,

    -- decision.Kind: choice / score / noul.
    kind            TEXT NOT NULL,

    -- The answer as JSON, marshalled by the server from decision.Answer.
    -- TEXT on both dialects, deliberately not JSONB: nothing in SQL reads
    -- inside it, and a jsonb column would be a second parser that refuses
    -- what the first one wrote (the 22P05 class migration 084 exists for).
    answer          TEXT NOT NULL,

    -- NULL where the primitive returns none — a Noul answer carries no
    -- confidence, and 0.0 would read as "certainly not".
    confidence      REAL,

    provider        TEXT NOT NULL,
    model           TEXT NOT NULL,
    state_hash      TEXT NOT NULL,
    item_seq        INTEGER NOT NULL,

    -- 1 when the provider truncated the state to fit its budget: the answer
    -- was computed from less than the item held.
    state_truncated INTEGER NOT NULL DEFAULT 0,

    evaluated_at    TEXT NOT NULL,

    FOREIGN KEY (item_id, workspace_id) REFERENCES items(id, workspace_id) ON DELETE CASCADE
);

-- Idempotency: one answer per question per state. A second evaluation of an
-- unchanged state finds this row and makes no call.
CREATE UNIQUE INDEX IF NOT EXISTS idx_item_decisions_state
    ON item_decisions(item_id, question_set, question_key, state_hash);

-- The read path: latest row per (set, key) for one item.
CREATE INDEX IF NOT EXISTS idx_item_decisions_latest
    ON item_decisions(item_id, question_set, question_key, evaluated_at);

-- The queue. ONE ROW PER (item, question_set): a burst of writes to one item
-- coalesces into a single owed evaluation rather than one call per write.
--
-- generation is what makes coalescing safe against a job that is already
-- running. Every enqueue bumps it; the runner records the generation it
-- claimed and deletes the row only if that is still the generation. A write
-- that lands mid-evaluation therefore leaves the row behind, and the next
-- tick evaluates the newer state — rather than the finished run deleting a
-- job it never saw.
--
-- claimed_by / lease_expires_at: a claim is honored until the lease expires,
-- after which any runner may take the row. A crashed runner strands nothing.
CREATE TABLE IF NOT EXISTS decision_jobs (
    item_id          TEXT NOT NULL,
    workspace_id     TEXT NOT NULL,
    question_set     TEXT NOT NULL,
    generation       INTEGER NOT NULL DEFAULT 1,
    enqueued_at      TEXT NOT NULL,
    claimed_by       TEXT,
    lease_expires_at TEXT,
    attempts         INTEGER NOT NULL DEFAULT 0,
    last_error       TEXT,

    PRIMARY KEY (item_id, question_set),
    FOREIGN KEY (item_id, workspace_id) REFERENCES items(id, workspace_id) ON DELETE CASCADE
);

-- The claim scan: oldest owed job first.
CREATE INDEX IF NOT EXISTS idx_decision_jobs_enqueued
    ON decision_jobs(enqueued_at);
