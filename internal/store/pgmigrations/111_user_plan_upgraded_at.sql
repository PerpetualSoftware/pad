-- BUG-3591: when an account last became Pro. The admin billing dashboard
-- counted "new Pro signups" by created_at, so a long-time free account that
-- upgraded inside the window was missed. SetUserPlan stamps this column in
-- the same UPDATE that makes the plan pro, under the row lock it already
-- holds, when the held row's effective plan was not pro. RFC3339 UTC, as
-- store.now writes it; '' means never upgraded (or upgraded before this
-- column existed: billing was pre-go-live, so there is no backfill).
ALTER TABLE users ADD COLUMN IF NOT EXISTS plan_upgraded_at TEXT NOT NULL DEFAULT '';
