-- TASK-2710: make "one collection per artifact kind, one invocation-routing
-- collection per workspace" a real database invariant instead of a best-effort
-- API gate. The enforcement-mechanism half of SPEC-0 L6, deferred from
-- TASK-2657 by lead ruling.
--
-- TASK-2657's checkTraitConflicts refuses duplicates at collection create and
-- update, and its own code says the gate is BEST-EFFORT: it reads then writes
-- without holding a lock across both, workspace import bypasses it by design
-- (warning instead, so a conflicting archive is never silent), and a rename
-- that frees a canonical slug can mint a duplicate with no write to that path
-- at all. With two declarations live, FindByArtifactKind returns whichever the
-- result order puts first — and that order is arbitrary, not a tie-break:
-- template-seeded collections share sort_order AND created_at, and on Postgres
-- the winner was measured FLIPPING BETWEEN RUNS on identical data.
--
-- The repair that has to happen first is NOT in this file. It is
-- dedupeTraitDeclarations (internal/store/trait_dedupe.go), which runs BEFORE
-- migrate() because these CREATE statements would fail on precisely the
-- databases that hold duplicates. It is in Go rather than SQL because the
-- ruling requires every resolution to be REPORTED — it silently changes which
-- collection owns a kernel behavior — and a SQL migration cannot log: Postgres
-- RAISE NOTICE goes nowhere here (no notice handler is registered) and SQLite
-- has no equivalent. Writing the rule once in Go and the indexes once in SQL
-- keeps one rule with one spelling.
--
-- Shape follows migrations/054, which already constrains items.invocation_slug
-- the same way: a partial unique index over an extracted JSON value, excluding
-- rows that do not opt in and rows that are soft-deleted.

-- One collection per artifact kind per workspace.
CREATE UNIQUE INDEX IF NOT EXISTS idx_collections_artifact_kind_per_workspace
    ON collections(workspace_id, json_extract(traits, '$.artifact_kind.kind'))
    WHERE json_extract(traits, '$.artifact_kind.kind') IS NOT NULL
      AND json_extract(traits, '$.artifact_kind.kind') != ''
      AND deleted_at IS NULL;

-- One invocation-routing collection per workspace. Unlike artifact_kind this
-- is not keyed by the value: the invariant is that at most ONE collection in a
-- workspace routes invocations at all, so the index is over workspace_id alone
-- and the partial predicate is what restricts it to the declaring rows.
CREATE UNIQUE INDEX IF NOT EXISTS idx_collections_invocation_field_per_workspace
    ON collections(workspace_id)
    WHERE json_extract(traits, '$.invocation_field') IS NOT NULL
      AND json_extract(traits, '$.invocation_field') != ''
      AND deleted_at IS NULL;
