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
// All read operations are synchronous — consumers don't `await`,
// they just read. `bootstrap` is async because it may hit IDB and
// the network; mutation methods (`upsert`, `applyDelta`, `remove`)
// are synchronous from the caller's perspective and write through
// to IDB in the background (fire-and-forget, never throwing — see
// `localIndexPersistence`).
//
// IDB persistence (TASK-1356): on bootstrap, hydrate from IDB FIRST
// for an immediate paint, then call `/items-changes?since=<idb-cursor>`
// to reconcile. On cache miss (IDB empty / unavailable), fall through
// to the cold-path `/items-index` fetch. Every mutation writes through
// to IDB so a reload picks up the latest state without a network
// round-trip. Storage failures are silently swallowed — the store
// keeps working in-memory.

import { SvelteMap } from 'svelte/reactivity';
import { api } from '$lib/api/client';
import { PadApiError } from '$lib/api/client';
import {
	hydrate as persistHydrate,
	persistDelta,
	persistRemovals,
	persistUpserts,
	wipe as persistWipe,
} from './localIndexPersistence';
import { localSearch } from './localSearch.svelte';
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

	// `userId` is captured on first bootstrap and used to scope the
	// IDB database name. Null = anonymous (pre-auth bootstrap). A
	// later bootstrap call with a different userId triggers a reset
	// (see `bootstrap`) so we never mix caches across users.
	userId: string | null = null;

	// `generation` is bumped on every `reset()`. Bootstrap captures
	// the value at start; if it advances during an await, the
	// in-flight bootstrap bails out instead of writing/reapplying
	// rows that belong to a stale identity. Without this, a sign-out
	// or 403 purge during a slow /items-index request would let the
	// completed snapshot resurrect just-purged rows (Codex P1
	// round 3).
	generation = 0;

	// `pendingResync` is true when the warm-cache path hydrated rows
	// from IDB but the follow-up /items-changes reconcile didn't
	// complete (transient network blip). The cache is usable —
	// `bootstrapState` is `'ready'` so the UI renders — but a later
	// `bootstrap()` call will retry the delta sync instead of
	// no-opping. Cleared on successful sync. Codex P2 round 5.
	pendingResync = $state(false);
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

/**
 * Apply a single row to a workspace's items map with the per-row
 * seq guard. Used by `bootstrap` (both warm and cold paths) and
 * `upsert`/`applyDelta` indirectly via the existing inline logic.
 * Returns true if the row was written, false if it was skipped as
 * stale.
 */
function mergeRow(state: WorkspaceState, row: ItemIndexRow | Item): boolean {
	const next = toSkinny(row);
	const existing = state.items.get(next.id);
	if (
		existing?.seq !== undefined &&
		next.seq !== undefined &&
		next.seq <= existing.seq
	) {
		return false;
	}
	state.items.set(next.id, next);
	return true;
}

/**
 * Rebuild the per-workspace MiniSearch index from the current in-RAM
 * snapshot. Called after the warm-cache hydrate and after the cold-path
 * /items-index settle — both are bulk row inserts where calling
 * `localSearch.upsert` per row would needlessly tear down and rebuild
 * the inverted index N times. A single `rebuild()` at the end is O(N)
 * and runs in <50ms for 5,000 rows.
 *
 * Steady-state mutations (`applyDelta`, `upsert`, `remove`) update the
 * search index incrementally inside those methods themselves.
 */
function rebuildSearchIndex(ws: string, state: WorkspaceState): void {
	localSearch.rebuild(ws, state.items.values());
}

export const localIndex = {
	/**
	 * Hydrate a workspace. Idempotent: returns the same in-flight
	 * promise if already loading; resolves immediately if already
	 * `'ready'`. On error the state flips to `'error'` and the caller
	 * can retry by calling `bootstrap` again. Archived items are
	 * included (the store is the canonical read model for both live
	 * and archived rows; consumers filter via `{ includeArchived }`).
	 *
	 * Two-stage flow (TASK-1356):
	 *
	 *   1. WARM PATH — hydrate from IDB. If the cache is populated,
	 *      copy rows into the in-RAM store, set the cursor from the
	 *      meta row, and flip `bootstrapState` to `'ready'` *before*
	 *      any network IO. The UI paints instantly. Then kick off
	 *      `/items-changes?since=<cursor>` in the background and
	 *      apply the deltas via `applyDelta` (which write-throughs to
	 *      IDB on its own). A failed delta-sync doesn't move state
	 *      back to `'loading'` — the UI keeps working off the cached
	 *      data and the next reconnect retries.
	 *
	 *   2. COLD PATH — IDB miss / unavailable. Fall through to the
	 *      classic `/items-index` snapshot, then write the result to
	 *      IDB so the next visit is warm.
	 *
	 * Merge-not-clear semantics are preserved: in either path, rows
	 * are MERGED through the same per-row seq guard `upsert` uses,
	 * and the cursor only advances forward. An optimistic `upsert()`
	 * or SSE write that landed while bootstrap was in flight is
	 * never regressed.
	 */
	async bootstrap(
		ws: string,
		opts: { userId: string | null },
	): Promise<void> {
		// User-mismatch reset BEFORE the early-return checks. Otherwise
		// a different user signing into the same browser would inherit
		// the previous user's `ready` state and in-flight promise
		// (Codex P1 round 5). `ensureState` auto-creates a fresh
		// WorkspaceState after the reset.
		const prior = workspaces.get(ws);
		if (prior && prior.bootstrapState !== 'cold' && prior.userId !== opts.userId) {
			localIndex.reset(ws);
		}

		const state = ensureState(ws);
		// Early return for already-ready states ONLY when there's no
		// outstanding resync work. A transient delta-sync failure
		// (network blip) leaves `pendingResync = true`; the next
		// bootstrap call must retry the reconcile, not no-op. Codex P2
		// round 5.
		if (state.bootstrapState === 'ready' && !state.pendingResync) return;
		const pending = inflight.get(ws);
		if (pending) return pending;

		// `userId` is REQUIRED (not defaulted) so authenticated callers
		// can't silently land their cache in the shared `anon`
		// namespace. Pass null explicitly for pre-auth / public-share
		// flows. Codex P? (round 4) caught the leak risk.
		state.userId = opts.userId;
		// Capture reentry BEFORE flipping bootstrapState to 'loading'
		// — otherwise the `state.bootstrapState === 'ready'` check
		// below always reads false and the warm IDB hydrate runs again
		// on a pendingResync retry. That's bad because IDB writes are
		// fire-and-forget; re-reading rows whose RAM copy was just
		// removed but whose IDB delete hasn't landed would resurrect
		// them. Codex P2 round 7.
		const reentry =
			state.bootstrapState === 'ready' && state.pendingResync;
		// Only flip to 'loading' for first-time bootstrap. A reentry
		// keeps state='ready' throughout so the UI never blanks while
		// retrying the reconcile.
		if (!reentry) state.bootstrapState = 'loading';
		// Capture the generation at start. If `reset()` runs during
		// any await below, generation bumps; we then bail out before
		// re-applying rows or writing the snapshot back, otherwise
		// purged data could resurrect (Codex P1 round 3).
		const bootstrapGen = state.generation;
		const userId = state.userId;
		const isStale = () => state.generation !== bootstrapGen;

		// `slot.p` holds the in-flight promise so the IIFE body's
		// `finally` can do an identity check against it — see Codex
		// round 7 P2. We need a level of indirection (the object)
		// because a bare `const p = ...` puts `p` in the TDZ when the
		// suspended async body resumes and references it.
		const slot: { p: Promise<void> | null } = { p: null };
		slot.p = (async () => {
			try {
				// Stage 1: warm path. Always try IDB first. Skip
				// re-hydration when this is a `pendingResync` retry —
				// the in-RAM state is already authoritative for this
				// session; we just need to redo the reconcile.
				const cached = reentry
					? { items: [], cursor: state.cursor }
					: await persistHydrate(userId, ws);
				if (isStale()) return;
				// A populated cache is one we've successfully synced
				// from before — either there are rows, or the cursor
				// has moved off the "0" floor (empty workspaces /
				// guests with item-level grants legitimately have
				// zero rows but a real cursor). Both deserve the
				// warm-path fast boot. Codex P2 round 8.
				const cacheIsPopulated =
					cached.items.length > 0 || cursorAsNum(cached.cursor) > 0;
				const hasCache = reentry || cacheIsPopulated;
				if (!reentry && cacheIsPopulated) {
					for (const row of cached.items) {
						mergeRow(state, row);
					}
					if (cursorAsNum(cached.cursor) > cursorAsNum(state.cursor)) {
						state.cursor = cached.cursor;
					}
					// Rebuild the search index from the warm snapshot so
					// the collection page and CommandPalette can serve
					// results immediately on first paint — TASK-1363.
					// Bulk rebuild is cheaper than N per-row upserts.
					rebuildSearchIndex(ws, state);
					// Flip to ready immediately — the UI paints from
					// the cache while delta-sync runs. `pendingResync`
					// stays true until the reconcile finishes.
					state.bootstrapState = 'ready';
					state.pendingResync = true;
				}

				if (hasCache) {
					// Reconcile cache against server via /items-changes.
					// The endpoint is capped at DefaultItemChangesLimit
					// (5000) per response, so we loop until the cursor
					// stops advancing — otherwise a cache that's
					// behind by more than one page would only catch
					// up by 5000 rows and then `bootstrapState` would
					// pin at `ready` forever, with no later trigger
					// to fetch the rest (Codex P2 round 1).
					//
					// Auth/authz failures (403) must NOT be swallowed
					// — the cache is stale-by-permission and showing
					// it as live is a real correctness bug. Re-throw
					// 403 so the registered access-revoked handler
					// (TASK-1360) sees it; the cache reset is its job.
					// Other network blips are non-fatal — the cache
					// stands and the next reconnect retries.
					try {
						// Cap iterations defensively — a healthy server
						// drains in < 10 pages even for huge gaps; if
						// something pathological loops without cursor
						// advance, give up after 50 and let the user's
						// next visit retry.
						let caughtUp = false;
						for (let i = 0; i < 50; i++) {
							const since = state.cursor;
							const delta = await api.items.changes(ws, since);
							if (isStale()) return;
							if (delta.changes.length === 0 || delta.cursor === since) {
								// Server returned no new rows AND no
								// cursor advance — we're caught up.
								caughtUp = true;
								break;
							}
							localIndex.applyDelta(ws, delta.changes, delta.cursor);
							if (delta.cursor === since) {
								caughtUp = true;
								break;
							}
						}
						// Only mark fresh if the loop reached the
						// server cursor. Hitting the 50-page cap
						// without catching up leaves `pendingResync`
						// true so a later bootstrap call resumes
						// (Codex P2 round 6). 50 × 5000 = 250,000
						// rows; we don't expect to hit this in practice
						// but it's the difference between "stale
						// forever" and "next visit retries".
						if (caughtUp) state.pendingResync = false;
					} catch (err) {
						if (isStale()) return;
						// 401 (unauthorized — session expired) and 403
						// (forbidden — access revoked) both mean the
						// cached rows are no longer ours to display.
						// Drop the cache and re-throw so the caller's
						// redirect / purge handler can react. Other
						// errors stay transient — cache stands and the
						// next bootstrap() call retries the reconcile
						// because `pendingResync` is still true.
						if (
							err instanceof PadApiError &&
							(err.code === 'forbidden' || err.code === 'unauthorized')
						) {
							state.bootstrapState = 'error';
							state.pendingResync = false;
							state.items.clear();
							state.cursor = '0';
							// Drop the MiniSearch index in lockstep with
							// the cleared in-RAM rows so a stale search
							// result can't navigate the user to a
							// now-forbidden row — TASK-1363.
							localSearch.reset(ws);
							persistWipe(userId, ws).catch(() => undefined);
							throw err;
						}
						// Transient network failure. Cache stands and
						// state stays 'ready' so the UI keeps working.
						// `pendingResync` remains true so the next
						// bootstrap() call retries (Codex P2 round 5).
						// Permission revocation that doesn't change
						// row data is NOT covered here — TASK-1360 and
						// DOC-1342 decision #3 explicitly punt that in
						// favor of the 403-on-click purge path.
					}
				} else {
					// Stage 2: cold path. /items-index full snapshot.
					const resp = await api.items.listIndex(ws, {
						includeArchived: true,
					});
					if (isStale()) return;
					for (const row of resp.items) {
						mergeRow(state, row);
					}
					if (cursorAsNum(resp.cursor) > cursorAsNum(state.cursor)) {
						state.cursor = resp.cursor;
					}
					state.bootstrapState = 'ready';
					// Cold path is a full snapshot — nothing pending.
					state.pendingResync = false;
					// Rebuild the search index from the cold snapshot —
					// TASK-1363. Bulk rebuild is cheaper than N per-row
					// upserts and keeps the index hot for the first
					// keystroke.
					rebuildSearchIndex(ws, state);
					// Best-effort persist the cold snapshot to IDB so
					// the next visit is warm. We persist the POST-MERGE
					// in-memory rows (not raw `resp.items`), and use
					// the same atomic rows+cursor write applyDelta does.
					// Otherwise an SSE/applyDelta that landed during the
					// in-flight /items-index request could overwrite a
					// newer row in IDB while the cursor on disk pointed
					// past the gap, leaving the cache permanently stale
					// (Codex P1 round 2). Iterating state.items.values()
					// yields exactly the merged, winning rows.
					const snapshot: ItemIndexRow[] = [];
					for (const row of state.items.values()) snapshot.push(row);
					persistDelta(userId, ws, snapshot, state.cursor).catch(
						() => undefined,
					);
				}
			} catch (err) {
				if (isStale()) return;
				state.bootstrapState = 'error';
				throw err;
			} finally {
				// Identity-checked cleanup: only clear inflight if
				// THIS promise is the registered one. After a
				// `reset()` mid-bootstrap, a fresh bootstrap call can
				// re-occupy the slot before this stale promise's
				// `finally` runs; deleting unconditionally would
				// remove the new entry and let a duplicate bootstrap
				// start (Codex P2 round 7).
				if (slot.p && inflight.get(ws) === slot.p) inflight.delete(ws);
			}
		})();
		inflight.set(ws, slot.p);
		return slot.p;
	},

	/**
	 * Synchronous filtered read by collection slug. Returns a freshly
	 * allocated array on every call; rely on `$derived` upstream for
	 * memoization.
	 *
	 * Sorted `updated_at DESC, id ASC` to match the server's
	 * /items-index ordering — `SvelteMap` is insertion-ordered, so
	 * after live `upsert`/`applyDelta` writes the natural iteration
	 * order would diverge from the bootstrap snapshot. Sorting on
	 * read keeps consumers stable across mutation paths.
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
		// Server order is `updated_at DESC, id ASC`. Strings sort
		// correctly here because `updated_at` is an RFC3339 string —
		// lexicographic compare equals chronological compare.
		out.sort((a, b) => {
			if (a.updated_at !== b.updated_at) {
				return a.updated_at < b.updated_at ? 1 : -1;
			}
			if (a.id === b.id) return 0;
			return a.id < b.id ? -1 : 1;
		});
		return out;
	},

	/**
	 * Flat list of EVERY item in the workspace (all collections), as
	 * Item[] with empty `content`. This is the workspace-wide lookup the
	 * item-detail page's wiki-link resolver and the editor's `[[` link
	 * picker need — both match on title / ref / slug across all
	 * collections but never read the rich-text body. Reusing the
	 * already-hydrated read model here means a detail page resolves links
	 * with ZERO extra fetch (it replaced a 4.7MB full-content /items
	 * load). Mirrors getByCollection's archived filter + sort. Returns []
	 * when the workspace isn't hydrated yet — callers bootstrap() first.
	 */
	getAll(ws: string, opts?: { includeArchived?: boolean }): Item[] {
		const state = workspaces.get(ws);
		if (!state) return [];
		const includeArchived = opts?.includeArchived === true;
		const out: Item[] = [];
		for (const row of state.items.values()) {
			if (!includeArchived && row.deleted_at) continue;
			// content:'' — link resolution/picker never read the body;
			// the skinny index rows don't carry it. Matches how the
			// collection page adapts getByCollection rows to Item.
			out.push({ ...row, content: '' } as Item);
		}
		out.sort((a, b) => {
			if (a.updated_at !== b.updated_at) {
				return a.updated_at < b.updated_at ? 1 : -1;
			}
			if (a.id === b.id) return 0;
			return a.id < b.id ? -1 : 1;
		});
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

		const toPersist: ItemIndexRow[] = [];
		const toRemove: string[] = [];
		for (const change of changes) {
			if (change.seq !== undefined) {
				// Guard 2: row's seq vs. cursor floor.
				if (change.seq <= startCursorNum) continue;
				// Guard 3: row's seq vs. existing row.
				const existing = state.items.get(change.id);
				if (
					existing?.seq !== undefined &&
					change.seq <= existing.seq &&
					!change.moved_out
				) {
					// Existing wins in RAM. Include it in the
					// persist set so the IDB cursor we're about
					// to advance doesn't lap a row that may not
					// be durable yet (upsert's fire-and-forget
					// IDB write could still be pending / failed
					// — Codex P? round 4). One redundant put is
					// cheaper than a missing row on warm boot.
					// (A moved-out eviction is unconditional — it
					// removes the row regardless of stored seq.)
					toPersist.push(existing);
					continue;
				}
			}
			// `moved_out: true` (BUG-1675) means the item left this
			// caller's visibility (moved into a collection they can't
			// see). The row carries only id + seq, so we HARD-evict it
			// from RAM + search and queue the IDB delete into the same
			// atomic cursor-advance tx below (no resurrect on warm boot).
			if (change.moved_out) {
				state.items.delete(change.id);
				localSearch.remove(ws, change.id);
				toRemove.push(change.id);
				continue;
			}
			// `deleted: true` is the server's derived view of
			// `deleted_at != nil` — a SOFT delete. The row still carries
			// its full skinny payload (including `deleted_at`), so we
			// upsert it like any other change. Hiding archived rows
			// from default reads is `getByCollection`'s job; this layer
			// only manages the seq-ordered identity of the row. Hard
			// deletes (workspace GC / 403 purge) go through the
			// `remove()` method, not through this batch path.
			const { deleted: _d, ...rest } = change;
			const skinny = toSkinny(rest as ItemIndexRow);
			state.items.set(change.id, skinny);
			toPersist.push(skinny);
			// Keep the search index in lockstep with the canonical
			// store — TASK-1363. Soft-deleted rows still index (the
			// `_deleted` flag gates them out at search time) so a
			// `{ includeArchived: true }` query finds them.
			localSearch.upsert(ws, skinny);
		}
		state.cursor = newCursor;
		// Write-through to IDB. ATOMIC: rows + cursor land in a
		// single transaction so the persisted cursor can never
		// advance past rows that didn't make it to disk (Codex P2
		// round 1). Fire-and-forget; storage failures degrade to
		// in-memory only and never break the read path. Routed
		// through the workspace's captured `userId` so a different
		// user signing into the same browser sees their own cache.
		persistDelta(state.userId, ws, toPersist, newCursor, toRemove).catch(
			() => undefined,
		);
	},

	/**
	 * Classify an SSE event against the workspace cursor (TASK-1358).
	 * Doesn't write data — the SSE wire payload only carries event
	 * metadata (type, item_id, title, collection_slug, seq), not the
	 * full row, so the caller still needs to fetch the row data
	 * through `deltaSync` for the cases that need it.
	 *
	 * Return values:
	 *   - `'no-seq'`: event lacks `seq` (legacy publisher, non-item
	 *      event). Caller should fall back to a generic deltaSync.
	 *   - `'stale'`: event.seq is at or below the current cursor —
	 *      already applied (or covered by a prior batch). Drop it.
	 *   - `'contiguous'`: event.seq === cursor + 1. No gap. Caller
	 *      may still need to fetch the row data for this event
	 *      (SSE payload is metadata-only), but is guaranteed not to
	 *      miss any intermediate events.
	 *   - `'gap'`: event.seq > cursor + 1. The local index is behind
	 *      by `event.seq - cursor - 1` events. Caller MUST
	 *      `deltaSync` to backfill before this event's data lands,
	 *      or the cache will have holes.
	 *
	 * Does NOT advance the cursor. Cursor advancement happens through
	 * `applyDelta` (with real row data) so the IDB persistence
	 * invariant (cursor never overshoots persisted rows) stays sound.
	 */
	classifySSEEvent(
		ws: string,
		event: { seq?: number },
	): 'no-seq' | 'stale' | 'contiguous' | 'gap' {
		if (event.seq === undefined || event.seq === 0) return 'no-seq';
		const cursorNum = cursorAsNum(localIndex.cursorFor(ws));
		if (event.seq <= cursorNum) return 'stale';
		if (event.seq === cursorNum + 1) return 'contiguous';
		return 'gap';
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
		// Mirror the write to the per-workspace MiniSearch index so
		// the next `localSearch.search(...)` reflects the mutation
		// without waiting for a periodic rebuild — TASK-1363.
		localSearch.upsert(ws, next);
		// Write-through to IDB. Fire-and-forget; storage failures
		// degrade silently.
		persistUpserts(state.userId, ws, [next]).catch(() => undefined);
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
		// Drop the row from the MiniSearch index too so the next
		// `localSearch.search(...)` never returns a hard-removed id —
		// TASK-1363.
		localSearch.remove(ws, id);
		// Write-through hard-delete to IDB.
		persistRemovals(state.userId, ws, [id]).catch(() => undefined);
	},

	/**
	 * Bulk-remove every item whose `collection_slug` matches. Used by
	 * the 403-purge path (TASK-1360) when a collection-level fetch
	 * returns `forbidden` — the entire collection's grants are now
	 * gone, so every row from that collection should drop. The
	 * remaining workspace state (other collections) stays intact.
	 *
	 * Idempotent — no-op when the workspace state isn't loaded or no
	 * rows match.
	 */
	removeByCollection(ws: string, collSlug: string): void {
		const state = workspaces.get(ws);
		if (!state) return;
		const ids: string[] = [];
		for (const row of state.items.values()) {
			if (row.collection_slug === collSlug) ids.push(row.id);
		}
		for (const id of ids) {
			state.items.delete(id);
			// Mirror each removal into the MiniSearch index so a
			// 403-purge collection wipe doesn't leave dangling
			// search hits — TASK-1363.
			localSearch.remove(ws, id);
		}
		if (ids.length > 0) {
			persistRemovals(state.userId, ws, ids).catch(() => undefined);
		}
	},

	/**
	 * Look up an item by EITHER id OR slug within a workspace. Used
	 * by the 403-purge path so a 403 on `/items/{slug}` can resolve
	 * the slug to the in-RAM id (the SvelteMap is keyed by id) and
	 * then drop the row. Returns null if no match.
	 */
	findByIdOrSlug(ws: string, idOrSlug: string): ItemIndexRow | null {
		const state = workspaces.get(ws);
		if (!state) return null;
		// Try id first (O(1) Map lookup).
		const byId = state.items.get(idOrSlug);
		if (byId) return byId;
		// Fall through to slug (O(n) scan — acceptable for the purge
		// path; collection-page reads use the id-keyed map).
		for (const row of state.items.values()) {
			if (row.slug === idOrSlug) return row;
		}
		return null;
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
		const prior = workspaces.get(ws);
		// Bump generation on the prior state object BEFORE deleting
		// it from the map. Any in-flight bootstrap promise still holds
		// a reference to `prior` — checking `state.generation !==
		// bootstrapGen` after each await lets it bail out instead of
		// writing or re-applying rows that belong to a stale identity
		// (Codex P1 round 3). Without this, a sign-out / 403 purge
		// during a slow /items-index request could resurrect just-
		// purged rows when the snapshot resolved.
		const priorUserId = prior?.userId ?? null;
		if (prior) prior.generation += 1;

		workspaces.delete(ws);
		inflight.delete(ws);

		// Drop the MiniSearch index for the workspace too. A fresh
		// bootstrap will rebuild it from the new owner's snapshot —
		// TASK-1363.
		localSearch.reset(ws);

		// Drop the persisted cache for the workspace's last-known
		// userId. If a different user later bootstraps the same
		// workspace, their cache is in a different IDB namespace and
		// remains untouched, by design.
		persistWipe(priorUserId, ws).catch(() => undefined);
	},

	/** Number of items currently held for a workspace. Test/debug aid. */
	size(ws: string): number {
		return workspaces.get(ws)?.items.size ?? 0;
	},
};
