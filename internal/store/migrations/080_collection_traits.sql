-- Collection kernel traits (SPEC-5 §Collection traits, PLAN-2656 phase 0 /
-- TASK-2657). Three kernel behaviors — what the agent bootstrap loads, which
-- items route by invocation slug, and which items export as portable
-- artifacts — were keyed on the literal collection slugs 'conventions' and
-- 'playbooks'. A slug is not a stable identifier (UpdateCollection re-slugs on
-- any name change), so renaming either collection silently detached all three
-- behaviors from it: BUG-2702. A trait travels with the collection.
--
-- Its own column rather than a key inside `schema` deliberately: the schema
-- column is overwritten wholesale on update and every client rebuilds it
-- fields-only, so a traits key stored there is destroyed by any ordinary
-- collection edit (measured during TASK-2657). Trait authority cannot rest on
-- a value an unrelated UI save deletes.
ALTER TABLE collections ADD COLUMN traits TEXT NOT NULL DEFAULT '{}';

-- Backfill: declare on existing workspaces the traits that the kernel used to
-- infer from these slugs, so behavior is unchanged across the upgrade.
--
-- Necessarily slug-keyed — the slug is the only identifier these rows carry,
-- and there is no record of what a collection used to be called. That means
-- this backfill does NOT reach a workspace that already renamed its
-- conventions collection before upgrading. It cannot do worse than the status
-- quo, because the status quo is itself slug-keyed and already broken for
-- exactly those workspaces (BUG-2702). From here forward the hazard is
-- structurally gone: the trait moves with the collection.
--
-- Guarded on traits = '{}' so a re-run, or a workspace that somehow already
-- carries declarations, is never overwritten.

-- Conventions declares TWO bootstrap includes. The bodies include is the
-- always-on rule set every agent must follow; the metadata include is the
-- body-less index of every ACTIVE convention (all triggers), so triggered
-- rules are discoverable without their bodies flooding the boot payload.
-- status=active appears in both: the pre-trait implementation filtered on it,
-- and omitting it would leak draft conventions into agent boot.
UPDATE collections
SET traits = json('{"bootstrap_include":[{"mode":"bodies","filter":{"status":"active","trigger":"always"},"key":"conventions"},{"mode":"metadata","filter":{"status":"active"},"key":"convention_index"}],"artifact_kind":{"kind":"convention"}}')
WHERE slug = 'conventions'
  AND (traits IS NULL OR traits = '' OR traits = '{}');

-- Playbooks declares one metadata include with NO filter. Draft and deprecated
-- playbooks are listed today, deliberately — an agent needs to see that a
-- half-written playbook exists (and the run gate refuses non-active ones
-- separately, BUG-2020). invocation_field marks this collection as routing by
-- invocation slug; v1 constrains the value to the literal 'invocation_slug'
-- because the partial unique indexes that guard uniqueness name that field.
UPDATE collections
SET traits = json('{"bootstrap_include":[{"mode":"metadata","key":"playbooks"}],"invocation_field":"invocation_slug","artifact_kind":{"kind":"playbook"}}')
WHERE slug = 'playbooks'
  AND (traits IS NULL OR traits = '' OR traits = '{}');
