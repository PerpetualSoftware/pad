-- Migration 151: verification evidence tied to a revision (TASK-3617; the
-- design note and its day-92 rulings are on that item's trail).
--
-- One row is one verdict on one acceptance criterion of the item that CARRIES
-- the criteria (a spec or plan), not of the task that did the work: that task
-- is via_item_id. Rows are append-only and never updated. "Latest per
-- criterion" is a query, and freshness is computed at read time, never stored.
--
-- WHY A TABLE AND NOT A METADATA KEY beside implementation_notes: a spec
-- re-verified per criterion per revision grows without bound, and the fields
-- JSON is read on every item fetch, list and SSE event and is seq-guarded on
-- write. item_decisions (090) made the same choice for the same reasons.
--
-- WHY criterion_hash AND NOT spec_seq FOR FRESHNESS. items.seq moves on writes
-- that do not touch the criterion (a status flip, an edit to another section),
-- so keying on it would stale every verification on unrelated churn.
-- criterion_hash is sha256 of the criterion's text as acceptance.Parse reads
-- it, so rewording the criterion stales its evidence and nothing else does.
-- spec_seq is kept for the audit: which version of the item the verdict saw.
--
-- Timestamps are fixed-width UTC TEXT (decisionTime), so TEXT order is time
-- order.
CREATE TABLE IF NOT EXISTS item_verifications (
    id              TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL,
    item_id         TEXT NOT NULL,

    -- "AC-N", a non-placeholder criterion of the item at write time.
    criterion_id    TEXT NOT NULL,
    criterion_hash  TEXT NOT NULL,
    spec_seq        INTEGER NOT NULL,

    -- Full 40-hex commit SHA, or '' when the criterion is not about code or
    -- the verifier's tree was dirty (a verdict on uncommitted code names no
    -- revision).
    code_revision   TEXT NOT NULL DEFAULT '',

    -- pass / fail / cant_tell: the verify playbook's three.
    verdict         TEXT NOT NULL,
    evidence        TEXT NOT NULL DEFAULT '',
    evidence_url    TEXT NOT NULL DEFAULT '',

    -- The item that did the work, when one is named. SET NULL rather than
    -- CASCADE: deleting the task must not delete the spec's evidence.
    via_item_id     TEXT REFERENCES items(id) ON DELETE SET NULL,

    -- Groups the rows one verify pass wrote.
    run_id          TEXT NOT NULL,

    -- The actor as the server stamps it. Self-declared like every created_by.
    created_by      TEXT NOT NULL DEFAULT '',
    user_id         TEXT NOT NULL DEFAULT '',
    agent_name      TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,

    FOREIGN KEY (item_id, workspace_id) REFERENCES items(id, workspace_id) ON DELETE CASCADE
);

-- The read path: latest row per criterion for one item.
CREATE INDEX IF NOT EXISTS idx_item_verifications_latest
    ON item_verifications(item_id, criterion_id, created_at);

CREATE INDEX IF NOT EXISTS idx_item_verifications_workspace
    ON item_verifications(workspace_id);

CREATE INDEX IF NOT EXISTS idx_item_verifications_via
    ON item_verifications(via_item_id);
