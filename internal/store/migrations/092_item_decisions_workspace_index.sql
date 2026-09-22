-- Item decisions workspace index (PLAN-3114 unit 3, TASK-3118).
--
-- The dashboard reads the latest `attention` answer for every item in a
-- workspace in one query. Before this index, the only way to find a
-- workspace's rows was a scan of the whole table, and the table keeps every
-- superseded answer for the audit, so it grows with each re-evaluation.
CREATE INDEX IF NOT EXISTS idx_item_decisions_workspace
    ON item_decisions(workspace_id, question_set);
