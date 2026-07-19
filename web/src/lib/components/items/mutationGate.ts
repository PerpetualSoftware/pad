// PLAN-2154 Phase 2 / D2 (TASK-2172): the master-freeze gate predicate.
//
// The full-page item view (`[slug]/+page.svelte`) keeps its `ItemDetail`
// master ALIVE while a detail pane peeks beside it (retain-alive — the collab
// provider is never torn down; no flush, snapshot, or persistence barrier).
// The master goes read-only via a single `peeking` prop. `mutationsEnabled`
// is the derived flag that gates EVERY local/user-originated mutation surface
// on the master: title edit, fields, assignment, move, delete, relationship
// add/remove, ChildItems add-child/reorder, the raw-markdown editor, the
// editor bubble/link popovers, the timeline (comment compose + edit forms +
// version restore), and the AttachmentImage node toolbar (via `editable`).
//
// Factored out as a pure function so the gate is a single, unit-testable
// source of truth shared by `ItemDetail.svelte` and its freeze test probe —
// the two can't drift. Keep it dependency-free.
//
// `peeking` defaults falsy at every non-peeking call site, so
// `mutationsEnabled === canEdit` for every existing (non-host) caller — the
// prop is a pure addition and leaves those callers byte-identical.
export function computeMutationsEnabled(canEdit: boolean, peeking: boolean): boolean {
	return canEdit && !peeking;
}
