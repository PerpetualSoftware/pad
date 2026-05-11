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
// Reactivity: items is a `SvelteMap` (from `svelte/reactivity`) so any
// `$derived` / `$effect` over `getByCollection` re-runs when the store
// changes. The per-workspace map itself is `$state`-wrapped so swapping
// workspaces also triggers downstream reactivity.
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

interface WorkspaceState {
	items: SvelteMap<string, ItemIndexRow>;
	cursor: string;
	bootstrapState: BootstrapState;
	// In-flight bootstrap promise so concurrent callers coalesce instead
	// of firing parallel /items-index requests.
	inflight: Promise<void> | null;
}

// The state is `$state`-wrapped at the module level so reactive
// consumers (via the public getters below) re-render when a workspace
// is added or its bootstrapState flips.
const workspaces = $state(new Map<string, WorkspaceState>());

function ensureState(ws: string): WorkspaceState {
	let state = workspaces.get(ws);
	if (!state) {
		state = {
			items: new SvelteMap<string, ItemIndexRow>(),
			cursor: '0',
			bootstrapState: 'cold',
			inflight: null,
		};
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
function cursorGte(a: string, b: string): boolean {
	const an = Number(a);
	const bn = Number(b);
	const ai = Number.isFinite(an) ? an : 0;
	const bi = Number.isFinite(bn) ? bn : 0;
	return ai >= bi;
}

export const localIndex = {
	/**
	 * Hydrate a workspace from `/items-index`. Idempotent: returns the
	 * same in-flight promise if already loading; resolves immediately
	 * if already `ready`. On error the state flips to `'error'` and the
	 * caller can retry by calling `bootstrap` again (the state is
	 * cleared back to `cold` on retry-entry).
	 */
	async bootstrap(ws: string): Promise<void> {
		const state = ensureState(ws);
		if (state.bootstrapState === 'ready') return;
		if (state.inflight) return state.inflight;

		state.bootstrapState = 'loading';
		state.inflight = (async () => {
			try {
				const resp = await api.items.listIndex(ws, { includeArchived: true });
				// Replace, don't merge — bootstrap is the authoritative
				// snapshot at this point. Anything in flight from SSE will
				// be reconciled on the next applyDelta via seq ordering.
				state.items.clear();
				for (const row of resp.items) {
					state.items.set(row.id, toSkinny(row));
				}
				state.cursor = resp.cursor;
				state.bootstrapState = 'ready';
			} catch (err) {
				state.bootstrapState = 'error';
				throw err;
			} finally {
				state.inflight = null;
			}
		})();

		return state.inflight;
	},

	/**
	 * Synchronous filtered read. Returns a freshly-allocated array on
	 * every call; rely on `$derived` upstream for memoization.
	 * Filters out items that are missing `collection_slug` (defensive —
	 * the index endpoint always populates it, but a typed
	 * non-null guard keeps the consumer code clean).
	 */
	getByCollection(ws: string, collSlug: string): ItemIndexRow[] {
		const state = workspaces.get(ws);
		if (!state) return [];
		const out: ItemIndexRow[] = [];
		for (const row of state.items.values()) {
			if (row.collection_slug === collSlug) out.push(row);
		}
		return out;
	},

	/**
	 * Apply a batch of changes from `/items-changes`. Upserts on
	 * non-deleted rows, removes on `deleted: true`. Cursor only
	 * advances forward — a server response that backslides (shouldn't
	 * happen, but harmless to defend) is ignored at the cursor level.
	 */
	applyDelta(ws: string, changes: ItemChangeRow[], newCursor: string): void {
		const state = ensureState(ws);
		for (const change of changes) {
			if (change.deleted) {
				state.items.delete(change.id);
			} else {
				const { deleted: _d, ...row } = change;
				state.items.set(change.id, toSkinny(row as ItemIndexRow));
			}
		}
		if (cursorGte(newCursor, state.cursor)) {
			state.cursor = newCursor;
		}
	},

	/**
	 * Single-item upsert. Used by SSE handlers and the optimistic
	 * post-mutation path (e.g. after `api.items.update` returns a full
	 * `Item`, the caller hands it here to keep the local index fresh
	 * without waiting for the SSE round-trip). Does NOT touch the
	 * cursor — that's the job of `applyDelta` / `applySSEEvent`.
	 */
	upsert(ws: string, row: ItemIndexRow | Item): void {
		const state = ensureState(ws);
		state.items.set(row.id, toSkinny(row));
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
	},

	/** Number of items currently held for a workspace. Test/debug aid. */
	size(ws: string): number {
		return workspaces.get(ws)?.items.size ?? 0;
	},
};
