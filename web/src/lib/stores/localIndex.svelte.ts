// localIndex — the in-RAM canonical store for the local-first read model
// (PLAN-1343 / TASK-1355). Per DOC-1342 design decision #4: the Svelte
// store owns truth in-RAM. IndexedDB persistence is bolted on in
// TASK-1356, but readers only ever talk to this store — the IDB layer
// is hydration + write-behind, never queried directly.
//
// Shape: one `WorkspaceState` per workspace slug, lazily created. Each
// holds:
//   - items: SvelteMap<itemId, ItemIndexRow>  — keyed by item.id
//   - cursor:        the highest workspace-scoped `seq` we have seen
//   - bootstrapState: 'cold' | 'loading' | 'ready' | 'error'
//
// Reactivity: the outer `workspaces` map is a `SvelteMap`, and
// `WorkspaceState` is a class whose `cursor` / `bootstrapState`
// fields are declared with the `$state` rune (Svelte 5 only allows
// `$state()` at variable-initializer, class-field, or
// constructor-first-assign sites — not as an arbitrary expression
// value inside a function). Its `items` is a `SvelteMap`, already
// reactive in its own right. The in-flight bootstrap promise lives
// in a separate plain Map — no reason to make a Promise reactive.
//
// Archived items (rows with `deleted_at` set on the server) are held
// alongside live items by design (see TASK-1357): the store is a
// workspace-wide read model, and the showArchived toggle is a
// render-time predicate. `getByCollection` filters them out by default;
// callers that want to render archived rows pass `{ includeArchived: true }`.
//
// On `/items-changes` deltas, `deleted: true` is the server's derived
// view of `deleted_at != nil` (a SOFT delete) — the row still carries
// its full skinny payload, so `applyDelta` upserts it like any other
// change. Hard deletes (workspace GC, 403 purge from TASK-1360) flow
// through `remove()` instead, which is the only path that drops a row
// id from the local index.
//
// Strip `content` defensively on every ingest path. The server's skinny
// `/items-index` and `/items-changes` endpoints already exclude the
// body, but `api.items.listIndex` / `api.items.changes` also strip
// the always-empty `content: ""` zero-value (see client.ts) — we do
// the same here so a caller passing a full `Item` (e.g. from
// `api.items.create` / `update`) cannot accidentally leak the rich
// body into the local index.
//
// All operations except `bootstrap` are synchronous — readers don't
// `await`, they just read.

import { SvelteMap } from 'svelte/reactivity';
import { api } from '$lib/api/client';
import type { Item, ItemChangeRow, ItemIndexRow } from '$lib/types';

export type BootstrapState = 'cold' | 'loading' | 'ready' | 'error';

// `WorkspaceState` is a class so its scalar fields can use the `$state`
// rune. `$state()` is not legal as an expression value inside a function
// in Svelte 5 — only at variable-initializer, class-field, or
// constructor-first-assign sites — so we can't lazily build a reactive
// plain object in `ensureState`. Wrapping the scalars in class fields
// gives us the same shape with reactivity intact. `items` is a
// `SvelteMap`, already reactive by itself.
class WorkspaceState {
	items: SvelteMap<string, ItemIndexRow> = new SvelteMap();
	cursor = $state('0');
	bootstrapState = $state<BootstrapState>('cold');
}

// Outer map: reactive (SvelteMap) so consumers re-render when a fresh
// workspace is hydrated. Each entry's WorkspaceState owns its own
// per-field reactivity via the class-field $state runes above.
const workspaces = new SvelteMap<string, WorkspaceState>();

// In-flight bootstrap promises live outside the reactive state — there
// is no reason to proxy a Promise, and keeping it separate makes the
// reactive-vs-internal split explicit.
const inflight = new Map<string, Promise<void>>();

function ensureState(ws: string): WorkspaceState {
	let state = workspaces.get(ws);
	if (!state) {
		state = new WorkspaceState();
		workspaces.set(ws, state);
	}
	return state;
}

/**
 * Strip a row down to the skinny shape. Defensive: discard `content`
 * if a caller passed a full `Item` rather than an `ItemIndexRow`. The
 * destructure-rest pattern produces a new shallow copy per call —
 * matches the discard-by-rest used in `api.items.listIndex` / `changes`.
 */
function toSkinny(row: ItemIndexRow | Item): ItemIndexRow {
	if ('content' in row) {
		const { content: _ignored, ...rest } = row as Item;
		return rest as ItemIndexRow;
	}
	return row;
}

/**
 * Cursors are decimal-encoded `seq` values as opaque strings — but
 * "monotonic forward" needs a numeric compare, not lexicographic.
 * Treat empty / non-numeric input as 0 so a fresh workspace's "0"
 * cursor compares correctly against a real response's "12345".
 */
function cursorAsNum(c: string): number {
	const n = Number(c);
	return Number.isFinite(n) ? n : 0;
}

export const localIndex = {
	/**
	 * Hydrate a workspace from `/items-index`. Idempotent: returns the
	 * same in-flight promise if already loading; resolves immediately
	 * if already `ready`. On error the state flips to `'error'` and
	 * the caller can retry by calling `bootstrap` again — the next
	 * call sees `bootstrapState === 'error'` and proceeds. Archived
	 * items are included in the snapshot (the store is the canonical
	 * read model for both live and archived rows; consumers filter
	 * via `{ includeArchived }`).
	 *
	 * Merge, don't clear. An optimistic `upsert()` or an SSE-driven
	 * write can land while the /items-index request is in flight; if
	 * we cleared and replaced, we'd silently regress those rows to
	 * the older snapshot value (Codex P2 round 3). Instead, we
	 * MERGE each response row through the same per-row seq guard
	 * `upsert` uses, and the cursor only advances forward. The
	 * cleared-state semantic isn't needed in practice because
	 * `reset()` is the explicit "drop everything" entry point.
	 */
	async bootstrap(ws: string): Promise<void> {
		const state = ensureState(ws);
		if (state.bootstrapState === 'ready') return;
		const pending = inflight.get(ws);
		if (pending) return pending;

		state.bootstrapState = 'loading';
		const p = (async () => {
			try {
				const resp = await api.items.listIndex(ws, { includeArchived: true });
				for (const row of resp.items) {
					const next = toSkinny(row);
					const existing = state.items.get(row.id);
					if (
						existing?.seq !== undefined &&
						next.seq !== undefined &&
						next.seq <= existing.seq
					) {
						continue;
					}
					state.items.set(row.id, next);
				}
				// Cursor moves forward only. A fresh /items-index can
				// occasionally return a cursor below the in-RAM one
				// (e.g. an SSE delta advanced it during the request);
				// don't backslide.
				if (cursorAsNum(resp.cursor) > cursorAsNum(state.cursor)) {
					state.cursor = resp.cursor;
				}
				state.bootstrapState = 'ready';
			} catch (err) {
				state.bootstrapState = 'error';
				throw err;
			} finally {
				inflight.delete(ws);
			}
		})();
		inflight.set(ws, p);
		return p;
	},

	/**
	 * Synchronous filtered read by collection slug. Returns a freshly
	 * allocated array on every call; rely on `$derived` upstream for
	 * memoization.
	 *
	 * By default, soft-deleted ("archived") rows are filtered out —
	 * the store holds them alongside live rows so a `showArchived`
	 * toggle doesn't need a refetch, but the typical view wants live
	 * only. Pass `{ includeArchived: true }` for archive views.
	 */
	getByCollection(
		ws: string,
		collSlug: string,
		opts?: { includeArchived?: boolean },
	): ItemIndexRow[] {
		const state = workspaces.get(ws);
		if (!state) return [];
		const includeArchived = opts?.includeArchived === true;
		const out: ItemIndexRow[] = [];
		for (const row of state.items.values()) {
			if (row.collection_slug !== collSlug) continue;
			if (!includeArchived && row.deleted_at) continue;
			out.push(row);
		}
		return out;
	},

	/**
	 * Apply a batch of changes from `/items-changes`. Always upserts
	 * — `deleted: true` on a change is the server's derived view of
	 * `deleted_at != nil` (a SOFT delete) and the row still carries
	 * its full skinny payload, so it gets stored alongside live rows
	 * with `deleted_at` populated. The default `getByCollection`
	 * filter hides those from live views; `{ includeArchived: true }`
	 * surfaces them. Hard deletes (workspace GC / 403 purge) go
	 * through `remove()` instead.
	 *
	 * Three guards against stale batches (all three caught by Codex
	 * across review rounds):
	 *
	 *   1. If `newCursor <= state.cursor`, drop the whole batch. The
	 *      server returns `cursor === since` on empty responses, so an
	 *      empty no-op trivially short-circuits here.
	 *   2. Per-row vs. cursor: skip changes whose `seq <= state.cursor`
	 *      at the START of the call. In normal /items-changes flow the
	 *      server filters to `seq > since`, but applyDelta is also a
	 *      public entry point (tests, future replay callers) — if any
	 *      row in a batch is stale, dropping it prevents overwriting
	 *      newer state.
	 *   3. Per-row vs. existing row: skip if there is already a row
	 *      with a higher `seq` in the store. `upsert()` and SSE
	 *      apply-event paths can store newer rows without touching the
	 *      cursor, so the cursor alone is not a sufficient floor —
	 *      a delta that legitimately advances the cursor can still
	 *      carry a row whose `seq` is older than what we already hold
	 *      for that id (e.g. SSE arrived first via a different path).
	 *
	 * Rows missing `seq` (legacy snapshots before TASK-1352) pass
	 * through unconditionally — there's no basis to compare. The
	 * cursor only advances forward, so a backslide can never lose
	 * progress.
	 */
	applyDelta(ws: string, changes: ItemChangeRow[], newCursor: string): void {
		const state = ensureState(ws);
		const startCursorNum = cursorAsNum(state.cursor);
		const newCursorNum = cursorAsNum(newCursor);

		// Guard 1: whole-batch drop on non-advancing cursor.
		if (newCursorNum <= startCursorNum) return;

		for (const change of changes) {
			if (change.seq !== undefined) {
				// Guard 2: row's seq vs. cursor floor.
				if (change.seq <= startCursorNum) continue;
				// Guard 3: row's seq vs. existing row.
				const existing = state.items.get(change.id);
				if (
					existing?.seq !== undefined &&
					change.seq <= existing.seq
				) {
					continue;
				}
			}
			// `deleted: true` is the server's derived view of
			// `deleted_at != nil` — a SOFT delete. The row still carries
			// its full skinny payload (including `deleted_at`), so we
			// upsert it like any other change. Hiding archived rows
			// from default reads is `getByCollection`'s job; this layer
			// only manages the seq-ordered identity of the row. Hard
			// deletes (workspace GC / 403 purge) go through the
			// `remove()` method, not through this batch path.
			const { deleted: _d, ...row } = change;
			state.items.set(change.id, toSkinny(row as ItemIndexRow));
		}
		state.cursor = newCursor;
	},

	/**
	 * Single-item upsert. Used by SSE handlers and the optimistic
	 * post-mutation path (e.g. after `api.items.update` returns a full
	 * `Item`, the caller hands it here to keep the local index fresh
	 * without waiting for the SSE round-trip). Does NOT touch the
	 * cursor — that's the job of `applyDelta` / `applySSEEvent`.
	 *
	 * Same per-row stale guard as `applyDelta`: if the incoming row's
	 * `seq` is not strictly greater than the existing row's `seq`,
	 * skip the write. Without this, a late SSE / out-of-order optimistic
	 * response could regress a row after a fresher version had already
	 * landed. Rows or peers missing `seq` (legacy snapshots before
	 * TASK-1352) overwrite unconditionally — there's no basis to
	 * compare.
	 */
	upsert(ws: string, row: ItemIndexRow | Item): void {
		const state = ensureState(ws);
		const next = toSkinny(row);
		const existing = state.items.get(row.id);
		if (
			existing?.seq !== undefined &&
			next.seq !== undefined &&
			next.seq <= existing.seq
		) {
			return;
		}
		state.items.set(row.id, next);
	},

	/**
	 * Single-item delete by id. Used by SSE archive/delete events and
	 * the 403-purge path (TASK-1360). Idempotent — removing a missing
	 * id is a no-op.
	 */
	remove(ws: string, id: string): void {
		const state = workspaces.get(ws);
		if (!state) return;
		state.items.delete(id);
	},

	/** Current cursor for a workspace, or "0" if unhydrated. */
	cursorFor(ws: string): string {
		return workspaces.get(ws)?.cursor ?? '0';
	},

	/**
	 * Current bootstrap state. Used by route loaders to decide whether
	 * to render a spinner, the items, or an error. Returns `'cold'`
	 * for unknown workspaces so first-visit consumers see a sane
	 * initial value.
	 */
	bootstrapStateFor(ws: string): BootstrapState {
		return workspaces.get(ws)?.bootstrapState ?? 'cold';
	},

	/**
	 * Drop all state for a workspace. Used by the 403-purge path
	 * (TASK-1360) when membership is revoked, and by the
	 * sign-out flow to keep the next user from seeing the previous
	 * user's cache. After reset, `bootstrap(ws)` from cold.
	 */
	reset(ws: string): void {
		workspaces.delete(ws);
		inflight.delete(ws);
	},

	/** Number of items currently held for a workspace. Test/debug aid. */
	size(ws: string): number {
		return workspaces.get(ws)?.items.size ?? 0;
	},
};
