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

import { SvelteMap, SvelteSet } from 'svelte/reactivity';
import { api } from '$lib/api/client';
import { PadApiError } from '$lib/api/client';
import {
	hydrate as persistHydrate,
	persistDelta,
	persistRemovals,
	persistReplace,
	persistAccessEpoch,
	persistRetag,
	persistUpserts,
	wipe as persistWipe,
} from './localIndexPersistence';
import { preserveProjectionMetadata, mergeEqualSeqProjection } from './itemRowMerge';
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
	// Mirrors the explicit capability bit on index/delta responses. It is
	// persisted with the cursor because the same user/workspace cache can
	// outlive a permission upgrade or downgrade.
	includesUnparentedMetadata = $state<boolean | null>(null);
	// The caller's access fingerprint as of the last authoritative snapshot or
	// delta (IDEA-2898). Persisted with the cursor for the same reason
	// `includesUnparentedMetadata` is: the cache outlives a permission change,
	// and a revocation that writes no row leaves no other trace.
	//
	// Null means NO BASELINE, and that outlives the first response: a server
	// that sends no epoch never supplies one, and the guard below declines to
	// adopt until the durable cache has been read. A null baseline over a
	// populated cache resyncs, which is the safe reading of "we cannot know
	// what this was authorised for".
	accessEpoch = $state<string | null>(null);
	// Has the DURABLE cache been read yet this session? (IDEA-2898, review of
	// the reduced tip.) `accessEpoch === null` and `items.size === 0` say what
	// RAM holds, which is not the same claim: during `bootstrap`'s hydrate
	// await, RAM is empty and the cache on disk may be full. Adopting an epoch
	// on that evidence is the silent adopt this change exists to prevent — see
	// `ensureAccessScope`.
	cacheRead = $state(false);
	// Does the durable cache reflect the MOST RECENT authoritative snapshot this
	// session installed in RAM? (TASK-2906 round 2; widened by TASK-2909.)
	//
	// Read it as a statement about the two stores AGREEING, not as the return
	// value of the last `persistReplace` call. `resyncProjectionScope` clears it
	// the moment it installs a snapshot in RAM and sets it again from the
	// persist result, so it is false for the whole window in between.
	//
	// TWO CONSUMERS, AND `markCaughtUp` IS NOT ONE OF THEM: `durableEpochFor`,
	// which stops vouching for the epoch while this is false, and
	// `ensureAccessScope`, which skips its durable stamp on the same evidence.
	// The ASK is deliberately not conditioned on it — see `markCaughtUp` for why
	// requiring disk there is a wedge rather than a guard. A resync that never
	// installs anything (the fetch threw, or the generation changed before the
	// snapshot was applied) leaves this alone, because RAM and IDB still agree.
	//
	// `persistReplace` can decline — its cursor lost a race to another tab, or
	// storage failed — and when it does the durable rows still describe the OLD
	// scope while RAM has moved on. `ensureAccessScope` must not then stamp the new epoch onto disk
	// on that resync's behalf: an epoch is a claim about the rowset it was
	// fetched with, and adopting one over a rowset that was never replaced makes
	// the cache agree with the server about a scope it does not hold — the
	// silent adopt IDEA-2898 exists to prevent, arriving through a different
	// door. Starts true: with no resync yet there is no unconfirmed snapshot.
	durableSnapshotCommitted = $state(true);

	// `scopeEpoch` bumps every time a projection resync STARTS installing a new
	// authoritative snapshot (i.e. the scope changed). Its consumer is the
	// optimistic-write fence in `upsert`: a mutation captures it when the
	// request is issued, and a write authorised under a superseded scope is
	// refused on return. Not persisted; a session-local ordering token.
	//
	// THE RECONCILE LOOP DOES NOT USE THIS — it uses the module-level
	// `reconcileTokens` map (TASK-2909; moved out of the state by IDEA-2913).
	// Plural until TASK-2921, which made the two copies one `reconcileWorkspace`.
	// They did until then, which is what made a settle bump here look free: it
	// would have answered their question and silently broken this one, refusing
	// writes issued after the new snapshot was already installed.
	scopeEpoch = 0;


	// `fencedIds` holds item ids the most recent resync DROPPED (absent from the
	// authoritative snapshot — hidden by a downgrade or deleted). `upsert`
	// refuses to write a fenced id, so a stale old-scope optimistic
	// create/update response resolving after the resync can't resurrect a
	// now-hidden row (Codex P1 round 8). An authoritative new-scope `applyDelta`
	// un-fences an id (the server says it's visible again); the next resync
	// recomputes the set from scratch, so a re-upgrade clears it. Not persisted;
	// the race it guards is within a single session.
	fencedIds = new Set<string>();

	// `movedOutFloor` maps an item id to the `seq` at which THIS session consumed
	// a `moved_out` eviction for it (TASK-2920 / PLAN-2903 item 6). It is a
	// per-id SEQ FLOOR, not a blocklist, and the distinction is the whole design:
	// a row is refused only while the evidence offered for it is NOT NEWER than
	// the eviction, so a genuine re-add — which the server stamps with a fresh,
	// higher seq — is admitted without anything having to expire or be cleaned
	// up. Correctness therefore does not depend on any pruning.
	//
	// It exists because `applyDelta`'s `moved_out` branch is a HARD evict
	// (`state.items.delete`), which leaves no row for the ordinary
	// `existing.seq` guards to compare against. Every door that merges rows
	// FETCHED OR READ BEFORE the eviction was consumed therefore saw an absent
	// id and reinstated the row: the cold `/items-index` snapshot and the warm
	// IDB hydrate (both via `mergeRow`), the resync snapshot, and a stale
	// optimistic `upsert`.
	//
	// The values compared are `seq` against `seq`, which IS orderable —
	// strictly monotonic per workspace — unlike `access_epoch`, whose
	// unorderability is what defeated the fences on IDEA-2898's abandoned
	// branch. PLAN-2903's working rule asks that this be stated rather than
	// assumed, so: this fence is entitled to say "older".
	//
	// Not persisted, for the same reason `fencedIds` is not: the eviction it
	// records reaches IDB atomically with the cursor advance in the same
	// `persistDelta` call, so a reload finds the row already gone from disk and
	// the cursor already past it — there is no window on the other side to
	// guard. The map guards a within-session race only.
	// BOUNDED, and NOT by the `delete` on the authoritative re-add paths: that
	// lift fires only when an item comes BACK, which by definition never happens
	// for one that moved out permanently — so it prunes exactly the entries that
	// were already harmless and none of the ones that accumulate (codex round 1
	// P2, against the first draft of the comment above, which implied otherwise).
	// `noteMovedOut` caps the map; see `MOVED_OUT_FLOOR_CAP`.
	movedOutFloor = new Map<string, number>();

	// `pendingRetags` records collection renames (collection id → latest new
	// slug) seen via `retagCollection` (BUG-2601). A rename event can land
	// BEFORE this workspace's rows exist (SSE connects fast; bootstrap's warm
	// IDB hydrate hasn't run), and the hydrated rows then arrive carrying the
	// dead slug with nothing left to re-stamp them — a rename touches no
	// items, so no delta ever will. The warm-hydrate path applies (then
	// clears) these; authoritative server snapshots (cold /items-index,
	// projection resync) clear them unapplied — server rows already carry the
	// live slug, and re-applying a recorded rename over fresher server truth
	// could regress a newer slug. Latest-wins per collection id; not
	// persisted (the window it guards is within a single session).
	pendingRetags = new Map<string, string>();

	// The auth identity the pendingRetags were recorded under (codex round 2
	// P2): a cold state survives the bootstrap user-mismatch reset (that
	// check deliberately skips cold states), so a rename recorded pre-
	// bootstrap by user A could otherwise be applied to user B's warm cache
	// after a console logout + login. Recorded at retag time from the
	// caller's auth view; the warm-hydrate apply discards the intent when
	// it doesn't match the bootstrapping user.
	pendingRetagsUser: string | null = null;
}

// Outer map: reactive (SvelteMap) so consumers re-render when a fresh
// workspace is hydrated. Each entry's WorkspaceState owns its own
// per-field reactivity via the class-field $state runes above.
const workspaces = new SvelteMap<string, WorkspaceState>();

// In-flight bootstrap promises live outside the reactive state — there
// is no reason to proxy a Promise, and keeping it separate makes the
// reactive-vs-internal split explicit.
/**
 * Per-workspace count of `reset()` calls, outliving the state it drops. See
 * `resetGenerationFor` for why this cannot live on the state object.
 */
const resetGenerations = new Map<string, number>();

/**
 * Workspaces whose cache was dropped because ACCESS WAS REVOKED (TASK-2921,
 * codex round 1 P1). A `$state` set so the reading component re-renders.
 *
 * Module-level, OUTLIVING THE STATE IT DESCRIBES, for the same reason
 * `resetGenerations` and `reconcileTokens` are — and here the reason is not
 * subtle: on a 403 the API client's global access-revoked handler runs
 * `localIndex.reset(scope.workspace)` BEFORE the error reaches any caller, and
 * `reset` DELETES the workspace's state entry. Anything recorded on that entry
 * is gone before the UI can read it, so a per-workspace field cannot answer
 * "was this a revocation or is it still loading".
 *
 * That is why the collection route carried a page-local `deltaSyncFailed` flag,
 * and why deleting it in favour of `bootstrapState === 'error'` was WRONG — the
 * first draft of this unit did exactly that and left the route on "Loading…"
 * forever after a 403. The fix is not to put the flag back on the page: it is to
 * put it where every route can see it, which is here.
 *
 * Cleared when a fresh (non-reentry) bootstrap starts, which is what the
 * banner's Retry CTA triggers.
 */
const accessRevoked = new SvelteSet<string>();

/**
 * Per-workspace reconcile token — the value a reconcile loop captures when it
 * ISSUES an `/items-changes` request and hands back when it reports catch-up
 * (TASK-2909, moved out of `WorkspaceState` by IDEA-2913).
 *
 * It bumps on exactly two events, and they are the two ways a verdict computed
 * against the cursor as the caller saw it can be overtaken:
 *
 *   - a projection resync SETTLES, having pinned the cursor;
 *   - the workspace's rows are DROPPED (`markWorkspaceDropped`).
 *
 * OUTSIDE `workspaces`, for the same reason `resetGenerations` is (TASK-2877):
 * `reset()` deletes that entry, so any counter living on the state object
 * restarts at zero and a response issued against the OLD state finds a matching
 * value on the replacement. That is an ABA — the counter returns to a value the
 * caller had seen, by way of a different object — and no per-state value can
 * close it, because "unchanged" and "reset back to zero" are the same number.
 *
 * ONE counter rather than making callers capture this AND `resetGenerationFor`:
 * two values to compare is two values to keep in step, and the door that forgets
 * the second one is the next lapse. The question both would answer is a single
 * question — "is the state I measured still the state I am reporting to" — so it
 * gets a single value. Its readers are the reconcile path and
 * nothing else — `clearAskIfSettled`, the two loops, and the
 * `reconcileTokenFor` accessor they capture through — verified by grep before
 * the move, which is what makes moving it safe here and was not true of
 * `scopeEpoch`.
 */
const reconcileTokens = new Map<string, number>();

function bumpReconcileToken(ws: string): void {
	reconcileTokens.set(ws, (reconcileTokens.get(ws) ?? 0) + 1);
}

/**
 * Record that a workspace's rows are gone. EVERY path that drops them calls
 * this — `reset()`, which deletes the whole state entry, and `bootstrap()`'s
 * 401/403 branch, which clears the rows in place. Two call sites rather than
 * one because the operations genuinely differ; the shared helper is what makes
 * the pairing greppable, and `localIndexResetGeneration.svelte.test.ts` fails
 * if a third site starts clearing rows without it.
 */
function markWorkspaceDropped(ws: string): void {
	resetGenerations.set(ws, (resetGenerations.get(ws) ?? 0) + 1);
	// A drop invalidates any reconcile verdict in flight just as a settled
	// resync does — the rows the response was computed against are gone
	// (IDEA-2913). Bumping HERE rather than in `reset()` inherits this helper's
	// guarantee of being the single funnel every drop path calls, which
	// `localIndexResetGeneration.svelte.test.ts` already enforces.
	bumpReconcileToken(ws);
}

const inflight = new Map<string, Promise<void>>();
const projectionResyncs = new Map<string, Promise<void>>();

function ensureState(ws: string): WorkspaceState {
	let state = workspaces.get(ws);
	if (!state) {
		state = new WorkspaceState();
		workspaces.set(ws, state);
	}
	return state;
}

/**
 * Re-stamp `collection_slug` on every cached row of one collection
 * (BUG-2601). Matches by STABLE `collection_id` — never by old slug,
 * which a different collection may have re-owned by the time we hear
 * about the rename. Idempotent; rows already carrying `newSlug` are
 * skipped. Mirrors the `upsert` write-through (search + IDB).
 */
function applyRetag(ws: string, state: WorkspaceState, collectionId: string, newSlug: string): void {
	const retagged: ItemIndexRow[] = [];
	for (const [id, row] of state.items.entries()) {
		if (row.collection_id !== collectionId || row.collection_slug === newSlug) continue;
		const next = { ...row, collection_slug: newSlug };
		state.items.set(id, next);
		localSearch.upsert(ws, next);
		retagged.push(next);
	}
	if (retagged.length > 0) {
		// persistRetag, NOT persistUpserts: a rename is a field-level intent,
		// and persisting it as a whole-row snapshot would be refused by the
		// IDB seq guard whenever a newer delta has already landed — dropping
		// the rename with nothing left to reapply it, since a rename produces
		// no item delta and pendingRetags is in-memory only (BUG-2609).
		persistRetag(
			state.userId,
			ws,
			collectionId,
			retagged.map((r) => r.id),
			newSlug,
		).catch(() => undefined);
	}
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

// `preserveProjectionMetadata` and `mergeEqualSeqProjection` moved to the pure
// shared module `itemRowMerge.ts` (PLAN-2636 unit 2) so the IDB persistence
// layer can apply the same equal-seq merge without importing this Svelte store.
// Imported at the top of this file; RAM behaviour is unchanged (bit-identical
// move, pinned by the localIndex tests).

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
 * Is this row older evidence than an eviction this session already consumed?
 * (TASK-2920.)
 *
 * ONE function rather than the comparison written at each of the four write
 * doors, because a rule landing at one door and not its sibling is the failure
 * PLAN-2903 has now hit five times — including once inside this very file, where
 * `bootstrap`'s reconcile loop cleared `pendingResync` directly instead of going
 * through `clearAskIfSettled`. (That particular pair is gone: TASK-2921 made the
 * two reconcile loops one function. The lesson stands; the instance is history.)
 *
 * `<=`, not `<`: the eviction IS the row's change at that seq, so a row offered
 * AT the floor is the same news, not newer news. Only a strictly higher seq is
 * evidence the row came back.
 *
 * A row with NO `seq` is admitted. There is genuinely no basis to order it, and
 * this file already resolves that case the same way in `upsert` ("Rows or peers
 * missing `seq` overwrite unconditionally"). The cost is bounded to a legacy /
 * mixed-deployment server that stamps no seq; refusing instead would drop rows
 * permanently in that deployment, which is the worse of the two failures.
 */
/**
 * How many eviction floors one workspace keeps (TASK-2920, codex round 1 P2).
 *
 * 5000 is `DefaultItemChangesLimit` in `internal/store/items.go` — the server's
 * per-page cap on `/items-changes`, and the one that applies here: the client
 * method takes an optional `limit` and its one live caller — `reconcileWorkspace`,
 * which both doors go through since TASK-2921 — passes none. Sizing the map to a full page means a
 * bulk move — the only way to add entries quickly — can never evict the floors
 * it is itself in the middle of recording. At a 36-byte uuid key plus a number
 * that is well under 1 MB per workspace, and it is session-local.
 *
 * Oldest-first eviction, which is the direction that matters: an entry's whole
 * job is to outlive row sets FETCHED BEFORE IT, so the oldest entry is the one
 * whose racing fetches are likeliest to have long since landed.
 *
 * WHAT THE CAP DOES NOT CLOSE, stated plainly because a bound that reads as a
 * guarantee is worse than one that reads as a bound (codex round 2 P2). The
 * reconcile loop PAGES, so more than one page of evictions can be consumed
 * inside a single in-flight snapshot; past 5000 of them the oldest floors are
 * gone before that snapshot merges, and those ids can be reinstated exactly as
 * they were before this fence existed. Every id under the cap is still
 * protected, so the cap strictly reduces the exposure and never widens it — but
 * it does not eliminate it.
 *
 * Closing it needs a different mechanism, not a bigger number: the cold path
 * would have to PIN its cursor to the snapshot's the way `resyncProjectionScope`
 * already does, so the replay re-delivers every eviction in the gap and no
 * per-id record is load-bearing at all. That is a change to the cold path's
 * cursor contract and it interacts with TASK-2906's durable monotonicity gate,
 * so it is filed rather than smuggled in here — see TASK-2920's trail.
 */
const MOVED_OUT_FLOOR_CAP = 5000;

/**
 * Record that an eviction for `id` was consumed at `seq`, keeping the map
 * bounded (TASK-2920).
 *
 * Highest wins, because the floor must never regress. A floor RAISED in place
 * deliberately keeps its original insertion position rather than moving to the
 * end: `Map` iteration order is what the cap evicts by, and a re-raise is the
 * same eviction learned about twice, not a newer one to give a fresh lease.
 */
function noteMovedOut(state: WorkspaceState, id: string, seq: number): void {
	const floor = state.movedOutFloor.get(id);
	if (floor !== undefined) {
		if (seq > floor) state.movedOutFloor.set(id, seq);
		return;
	}
	state.movedOutFloor.set(id, seq);
	while (state.movedOutFloor.size > MOVED_OUT_FLOOR_CAP) {
		const oldest = state.movedOutFloor.keys().next();
		if (oldest.done) break;
		state.movedOutFloor.delete(oldest.value);
	}
}

function refusedByMovedOut(state: WorkspaceState, row: ItemIndexRow | Item): boolean {
	const floor = state.movedOutFloor.get(row.id);
	if (floor === undefined) return false;
	const seq = (row as ItemIndexRow).seq;
	if (seq === undefined) return false;
	return seq <= floor;
}

/**
 * Apply a single row to a workspace's items map with the per-row
 * seq guard. Used by `bootstrap` (both warm and cold paths) and
 * `upsert`/`applyDelta` indirectly via the existing inline logic.
 * Returns true if the row was written, false if it was skipped as
 * stale.
 */
function mergeRow(state: WorkspaceState, row: ItemIndexRow | Item): boolean {
	if (refusedByMovedOut(state, row)) return false;
	let next = toSkinny(row);
	const existing = state.items.get(next.id);
	next = preserveProjectionMetadata(existing, next);
	if (
		existing?.seq !== undefined &&
		next.seq !== undefined &&
		next.seq < existing.seq
	) {
		return false;
	}
	if (existing?.seq !== undefined && next.seq === existing.seq) {
		const merged = mergeEqualSeqProjection(existing, next);
		if (!merged) return false;
		state.items.set(next.id, merged);
		return true;
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

/**
 * The access epoch a DELTA may stamp on the durable cache (TASK-2906 round 3).
 *
 * `state.accessEpoch` is what RAM believes, and after a REFUSED `persistReplace`
 * that belief is true of RAM and false of disk: the resync adopted the new epoch
 * while the durable rows were left describing the old scope. A delta advancing
 * the cursor would then carry the new epoch onto those rows and make the cache
 * agree with the server about a scope it does not hold — the same silent adopt
 * the joined-resync stamp was taught to skip, one door along. Skipping is not
 * available here, because the delta's rows and cursor must still land.
 *
 * So the delta DECLINES TO VOUCH: it passes `undefined`, which `persistDelta`
 * reads as "carry the stored epoch" (TASK-2909 review round 1 P2 — an explicit
 * null, which this shipped as, could commit after a replace and clobber the
 * epoch that replace had just recorded). The carried value keeps the durable
 * epoch agreeing with the durable ROWS, which is what makes the SERVER's next
 * epoch disagree with it and trigger a resync. It clears itself — the next
 * `persistReplace` that commits records the real epoch and flips the flag back.
 *
 * The repair is a LATER one, not an in-session one: it fires on the next
 * hydrate, or on the next response whose epoch disagrees. Nothing here repairs
 * the durable cache at the moment the replace is refused.
 *
 * One function rather than the expression written at each `persistDelta` call,
 * because a rule applied at one door and not its sibling is how this unit's
 * previous two rounds each went.
 */
/**
 * Clear the "this workspace still owes a replay" ask, if it may be cleared
 * (TASK-2909).
 *
 * ONE definition, two callers: the public `markCaughtUp` (used by reconcile
 * loops that own their own catch-up) and `bootstrap`'s internal loop. They used
 * to hold the decision separately, so a condition added to one did not reach
 * the other.
 *
 * Both doors pass the token their own loop captured at request time; this helper
 * owns the whole decision, so a condition added here reaches both.
 */
function clearAskIfSettled(ws: string, state: WorkspaceState, token: number): void {
	// The response was computed against the cursor as it stood when the request
	// started. A resync that overlapped it pinned the cursor since, so the
	// verdict is stale — however late it arrives, and whether or not anything is
	// still in flight now.
	if ((reconcileTokens.get(ws) ?? 0) !== token) return;
	// ...and the still-running case, which the token alone cannot catch: the
	// counter only moves when a resync SETTLES, so a response that arrives while
	// one is mid-flight still carries a matching token.
	if (projectionResyncs.has(ws)) return;
	state.pendingResync = false;
}

function durableEpochFor(state: WorkspaceState): string | null | undefined {
	// `undefined` is CARRY THE STORED EPOCH, not "no epoch" — see persistDelta's
	// param doc. Writing an explicit null here (TASK-2906's first shape) is
	// unsafe during the window between a resync installing its snapshot in RAM
	// and `persistReplace` resolving: a delta committing after that replace
	// would clobber the epoch it had just correctly recorded, and the next boot
	// would pay a full snapshot to rediscover it (review round 1 P2).
	return state.durableSnapshotCommitted ? state.accessEpoch : undefined;
}

/**
 * The workspace reconcile loop — ONE definition, both doors (TASK-2921).
 *
 * Drains `/items-changes` from the workspace cursor until it stops advancing,
 * routing a changed access epoch or projection scope through the PUBLIC
 * `ensureAccessScope` / `ensureProjectionScope` entry points, and clears the
 * outstanding ask through `clearAskIfSettled` when — and only when — an
 * iteration concludes catch-up.
 *
 * Returns true if it caught up, false if it hit the page cap. A caller that
 * needs to know is the collection route, whose banner distinguishes the two;
 * `bootstrap` ignores it, because the ask is the state that matters and this
 * function already owns it.
 *
 * WHY IT IS ONE FUNCTION. Until now `bootstrap` and the collection route's
 * `deltaSync` each carried their own copy, and the copies had already drifted
 * in four places: the route had no generation check, `bootstrap` reimplemented
 * `ensureProjectionScope`'s predicate inline, the two cleared the ask through
 * different doors, and only one assigned `includesUnparentedMetadata` per
 * iteration. Every rule this plan has added arrived at one copy first — the
 * comment this replaced said so about `clearAskIfSettled`, calling it the fourth
 * time a rule landed at one door and not its sibling.
 *
 * `isStale` is the caller's own "the state I captured is gone" predicate.
 * `bootstrap` passes its generation check; the route passes nothing, because a
 * page has no generation to compare against and its `reset()` path tears the
 * page down anyway. It is checked after EVERY await, which is stricter than
 * what the route had (nothing) and identical to what `bootstrap` had.
 *
 * ONE DELIBERATE BEHAVIOUR CHANGE, not a refactor artifact. `bootstrap`'s inline
 * projection test was `includesUnparentedMetadata !== null && !== incoming`, so
 * a NULL scope over a populated cache silently ADOPTED the incoming value.
 * `ensureProjectionScope` resyncs in that case instead, for the same reason the
 * access epoch's null baseline does: a cache whose scope we cannot vouch for
 * must not be told what it was authorised for by the response we are checking
 * it against. Routing both doors through the public entry point is the point of
 * the unit, and this is the one place where doing so changes an answer.
 */
/**
 * The reaction to a 401 / 403 from a reconcile: the cached rows are no longer
 * this caller's to display (TASK-1360). ONE definition, both doors (TASK-2921).
 *
 * `bootstrap` has done this since TASK-1360 and the collection route's
 * `deltaSync` did a WEAKER version of it — `localIndex.reset(ws)`, which deletes
 * the whole state entry. Now that the reconcile itself is shared, its failure
 * reaction has to be too, or moving the driver to the layout would have silently
 * dropped the purge on every route: the page was the only thing reacting, and
 * the page is no longer the thing running the loop.
 *
 * Deliberately NOT `reset()`. `reset()` deletes the state entry, which loses the
 * `'error'` bootstrapState the UI reads to tell "access revoked" from "still
 * loading" — the collection route carried a page-local `deltaSyncFailed` flag
 * purely to remember that across the deletion. Clearing in place keeps the
 * distinction in the store, where every route can see it.
 */
function dropCacheForAuthError(ws: string, state: WorkspaceState): void {
	// FIRST, and outside the state object: `reset` may already have deleted the
	// entry `state` points at (the API client's global 403 handler calls it
	// before the error reaches us), in which case every assignment below lands
	// on a detached object nobody will read. This one does not.
	accessRevoked.add(ws);
	state.bootstrapState = 'error';
	state.pendingResync = false;
	state.items.clear();
	state.cursor = '0';
	// This IS a drop, so it counts as one (TASK-2877): an in-flight write-back
	// resolving after this would otherwise resurrect rows into a workspace whose
	// access was just revoked. Bumped here rather than by funnelling through
	// `reset()`, which also deletes the state entry.
	markWorkspaceDropped(ws);
	// Drop the MiniSearch index in lockstep with the cleared in-RAM rows so a
	// stale search result can't navigate the user to a now-forbidden row
	// (TASK-1363).
	localSearch.reset(ws);
	persistWipe(state.userId, ws).catch(() => undefined);
}

/** Is this the error that means the cache is no longer ours to show? */
function isAuthError(err: unknown): boolean {
	return (
		err instanceof PadApiError &&
		(err.code === 'forbidden' || err.code === 'unauthorized')
	);
}

async function reconcileWorkspace(
	ws: string,
	state: WorkspaceState,
	isStale: () => boolean = () => false,
): Promise<boolean> {
	// Cap iterations defensively — a healthy server drains in < 10 pages even
	// for huge gaps; if something pathological loops without cursor advance,
	// give up after 50 and let the caller retry. 50 x 5000 = 250,000 rows. A cap
	// hit leaves the ask SET, which is the difference between "stale forever"
	// and "next visit resumes".
	for (let i = 0; i < 50; i++) {
		// Captured when the request is ISSUED — the staleness it guards against
		// is decided then, not when the response is read (TASK-2909).
		const reconcileToken = reconcileTokens.get(ws) ?? 0;
		const since = state.cursor;
		const delta = await api.items.changes(ws, since);
		if (isStale()) return false;
		if ((reconcileTokens.get(ws) ?? 0) !== reconcileToken) {
			// A concurrent resync installed a new snapshot and pinned the cursor
			// while this request was in flight, so this response predates it and
			// cannot confirm catch-up. Re-poll from the new cursor.
			continue;
		}
		// IDEA-2898. The caller's visible set changed with no row change to
		// carry it — a revocation writes no item, so nothing else in this
		// response could evict the rows it hid.
		//
		// `continue`, never `break`: the resync pins the cursor to the
		// snapshot's, so post-snapshot mutations are replayed only if the loop
		// polls again. Neither branch can re-fire, because each resync aligns
		// the value its check compares.
		if (await localIndex.ensureAccessScope(ws, delta.access_epoch)) {
			if (isStale()) return false;
			continue;
		}
		if (isStale()) return false;
		if (await localIndex.ensureProjectionScope(ws, delta.includes_unparented_metadata)) {
			if (isStale()) return false;
			continue;
		}
		if (isStale()) return false;
		if (delta.changes.length === 0 || delta.cursor === since) {
			// No new rows AND no cursor advance — caught up.
			clearAskIfSettled(ws, state, reconcileToken);
			return true;
		}
		localIndex.applyDelta(
			ws,
			delta.changes,
			delta.cursor,
			delta.includes_unparented_metadata,
		);
		if (delta.cursor === since) {
			clearAskIfSettled(ws, state, reconcileToken);
			return true;
		}
	}
	return false;
}

async function resyncProjectionScope(
	ws: string,
	state: WorkspaceState,
	// The epoch the SERVER most recently told this caller, when the caller has
	// one. Used only if the authoritative snapshot carries none — an older
	// server answering /items-index during a mixed deployment. Passing it in
	// rather than patching `state.accessEpoch` after the resync returns is what
	// keeps RAM and IDB agreeing: `persistReplace` happens inside this
	// function, so a post-hoc assignment would leave the durable meta row a
	// version behind the in-memory baseline. The next warm boot hydrates the
	// DURABLE one, so it would start from a baseline this session had already
	// superseded and resync on its first delta — a full snapshot per reload,
	// for a scope that never actually changed (round 2).
	fallbackEpoch?: string,
): Promise<void> {
	const pending = projectionResyncs.get(ws);
	if (pending) return pending;
	const generation = state.generation;
	const userId = state.userId;
	const promise = (async () => {
		// Mark the workspace as needing catch-up the instant a resync begins,
		// regardless of caller. A resync installs the snapshot and pins the
		// cursor, but the post-snapshot mutations aren't caught up until a
		// reconcile loop drains from that cursor. `reconcileWorkspace` — which
		// both doors go through since TASK-2921 — clears this only on caughtUp,
		// so a replay that fails or hits the 50-page cap keeps
		// pendingResync=true and the next bootstrap() resumes instead of
		// no-opping with racing mutations still missing. (A page resync that DOES
		// catch up leaves it set too — the next bootstrap runs one harmless,
		// idempotent reconcile pass and clears it.)
		state.pendingResync = true;
		// Advance the scope epoch NOW — before the network await, not after the
		// snapshot installs. A reconcile response that races this fetch must see
		// the epoch already changed so it can't declare catch-up and clear the
		// pendingResync we just set (Codex P2 round 9). The bump is monotonic and
		// side-effect-free: even if this resync later bails on a generation
		// mismatch, a spurious bump just makes in-flight loops re-poll once.
		state.scopeEpoch += 1;
		// Fetch the authoritative snapshot BEFORE touching any state. The
		// previous ordering cleared the in-RAM store, cursor, search index,
		// and IDB cache up front so a permission downgrade couldn't flash
		// stale projection bits during the in-flight fetch — but a transient
		// fetch failure then left the store permanently empty with no
		// rollback, blanking the user's entire item list until the next
		// re-bootstrap (looked like data loss). The stale rows were already
		// on screen before the resync triggered, and the server enforces the
		// actual access control (a restricted caller gets 403 on the
		// unparented filter regardless of these cosmetic bits), so deferring
		// the clear until the snapshot lands closes the data-loss hole
		// without widening any real exposure.
		const resp = await api.items.listIndex(ws, { includeArchived: true });
		if (state.generation !== generation) return;

		// Reconcile the authoritative snapshot into the live store, then let the
		// delta stream heal anything that raced the fetch — "drop absent, replay
		// under the new scope":
		//
		//   1. DROP every row absent from the snapshot. The snapshot IS the
		//      authoritative view under the (possibly downgraded) new scope, so a
		//      row it omits must not linger — otherwise an old-scope delta that
		//      raced the fetch could keep a now-hidden row alive, and no
		//      new-scope delta would ever evict it (the server no longer sends
		//      it). We deliberately do NOT preserve seq > snapshotCursor rows
		//      here: a post-snapshot mutation the client can still see is
		//      re-fetched by the very next `/items-changes?since=cursor` below,
		//      now filtered by the new scope, so visible rows return and hidden
		//      rows stay gone. Nothing is permanently lost.
		//   2. For rows PRESENT in the snapshot, keep a strictly-higher local seq
		//      (a still-visible racing edit) over the older snapshot copy;
		//      otherwise the snapshot row REPLACES the local copy outright,
		//      including dropping an is_unparented bit the new scope no longer
		//      grants. mergeRow is deliberately NOT used — its projection
		//      preservation is for the optimistic equal-seq case and would carry
		//      a stale projection bit across a downgrade.
		//   3. Set the cursor to the snapshot cursor (never advance past it) so
		//      step 1's replay actually re-fetches post-snapshot mutations.
		//
		// The map never empties — the full snapshot lands before any await — so
		// subscribers never see a blank list.
		const snapshotIds = new Set<string>();
		for (const row of resp.items) snapshotIds.add(row.id);
		const toDrop: string[] = [];
		for (const id of state.items.keys()) {
			if (!snapshotIds.has(id)) toDrop.push(id);
		}
		for (const id of toDrop) {
			state.items.delete(id);
			localSearch.remove(ws, id);
		}
		// FENCE the dropped ids so a stale old-scope optimistic upsert can't
		// resurrect a row this snapshot just hid. Replace, don't union: a later
		// re-upgrade snapshot won't list these ids in toDrop, clearing the fence
		// for them. (The scope epoch was already bumped before the fetch above.)
		state.fencedIds = new Set(toDrop);
		state.includesUnparentedMetadata = resp.includes_unparented_metadata;
		// The snapshot IS the new scope, so its epoch is the new baseline. Set
		// here rather than at the call sites: every route into a resync ends
		// here, and a baseline left stale would re-fire the resync on the very
		// next poll.
		//
		// ABSENT means UNKNOWN, and unknown must not erase what we know
		// (review round 1 F3). `?? null` here read "no epoch on the snapshot"
		// as "this workspace has no baseline", which during a mixed deployment
		// — a delta from a new server, a snapshot from an old one — sent the
		// reconcile loop into the null-baseline branch and resynced again, up
		// to the 50-page cap, a full snapshot per iteration. Keeping the prior
		// value leaves the loop's own termination to `ensureAccessScope`,
		// which records the epoch it was TOLD once a resync has run.
		if (resp.access_epoch !== undefined) {
			state.accessEpoch = resp.access_epoch;
		} else if (fallbackEpoch !== undefined) {
			state.accessEpoch = fallbackEpoch;
		}
		for (const row of resp.items) {
			// The snapshot can predate an eviction this session already consumed
			// (TASK-2920). A resync SELF-HEALS that case — it pins the cursor to
			// the snapshot's below, so the caller's next `/items-changes`
			// re-delivers the eviction — so the guard here closes a visible
			// flicker rather than a durable defect. It is applied anyway because
			// this is the same door under a different roof, and the alternative
			// is a rule that holds in `mergeRow` and not here.
			if (refusedByMovedOut(state, row)) continue;
			const next = toSkinny(row);
			const existing = state.items.get(next.id);
			if (
				existing?.seq !== undefined &&
				next.seq !== undefined &&
				existing.seq > next.seq
			) {
				// Still-visible racing edit — keep it, but strip a projection bit
				// the restricted new scope no longer grants (the snapshot copy
				// wouldn't carry it).
				if (!resp.includes_unparented_metadata && 'is_unparented' in existing) {
					const { is_unparented: _dropped, ...stripped } = existing;
					state.items.set(next.id, stripped);
				}
				continue;
			}
			state.items.set(next.id, next);
		}
		state.cursor = resp.cursor;
		// FROM HERE, RAM AND DISK DISAGREE UNTIL `persistReplace` SAYS OTHERWISE
		// (TASK-2909). Every exit below this line — the generation guard, a
		// refused replace, or simply the time the transaction takes — leaves the
		// durable cache describing the PREVIOUS snapshot, and both readers of
		// this flag need to know that: a delta must not stamp RAM's epoch onto
		// rows nobody replaced (`durableEpochFor`), and a joined caller must not
		// stamp it on the resync's behalf (`ensureAccessScope`). Setting it only
		// from the persist RESULT left it carrying the previous resync's verdict
		// across this whole window — which the persist-await test pins.
		state.durableSnapshotCommitted = false;
		state.bootstrapState = 'ready';
		// Server rows carry the live collection slug — a rename recorded
		// pre-snapshot is already reflected, and re-applying it later could
		// regress a newer slug (BUG-2601).
		state.pendingRetags.clear();
		// NOTE: deliberately do NOT clear pendingResync here. This resync only
		// installs the authoritative snapshot and pins the cursor for replay;
		// the post-snapshot mutations aren't caught up until the caller's delta
		// loop drains from the pinned cursor. `reconcileWorkspace` owns the flag
		// — whichever door is running it, it clears pendingResync only once
		// `caughtUp` is true (before TASK-2921 this said "the bootstrap reconcile
		// loop", which had already stopped being the only clearer when TASK-2099
		// gave the page `markCaughtUp`), so a
		// replay that later fails transiently or hits the 50-page cap leaves
		// pendingResync=true and the next bootstrap() resumes instead of
		// no-opping with racing mutations still missing.
		rebuildSearchIndex(ws, state);

		// Reconcile the persisted cache after the successful fetch via a single
		// transactional clear-and-replace: rows the resync dropped (e.g. after a
		// downgrade) must not survive in the cache, and persistReplace commits
		// the clear + the new snapshot atomically without the deleteDatabase()
		// cross-tab hang that a wipe()+persistDelta() pair risks. Recheck
		// generation first: a sign-out / 403 purge (reset()) after the fetch
		// bumps generation and clears the store, and without this guard the
		// write below would resurrect the pre-reset snapshot on the next warm
		// hydrate.
		if (state.generation !== generation) return;
		const snapshot = [...state.items.values()];
		state.durableSnapshotCommitted = await persistReplace(
			userId,
			ws,
			snapshot,
			state.cursor,
			resp.includes_unparented_metadata,
			// The state's baseline, NOT `resp.access_epoch ?? null` (round 2).
			// F3 deliberately keeps a known baseline when the snapshot carries
			// no epoch; writing null here would contradict it in the durable
			// copy. A null on disk is not a harmless gap: `ensureAccessScope`
			// reads a null baseline over a POPULATED cache as "cannot know what
			// this was authorised for" and resyncs, so the next warm boot would
			// pay a full snapshot to rediscover the epoch this one already
			// knew. The fix for an absent epoch on the wire must not become an
			// absent epoch on disk.
			state.accessEpoch,
		);
	})();
	projectionResyncs.set(ws, promise);
	try {
		await promise;
	} finally {
		if (projectionResyncs.get(ws) === promise) projectionResyncs.delete(ws);
		// BUMP THE OVERLAP COUNTER ON SETTLE (TASK-2909, review rounds 2-3).
		//
		// This is the ONLY bump. A reconcile loop captures the counter when its
		// `/items-changes` request starts, so bumping here makes every capture
		// taken before or during this resync mismatch once it finishes — which
		// is exactly the set of responses whose verdict the pinned cursor has
		// overtaken. The remaining case, a response that returns while this
		// resync is still running, has not reached this line yet and is refused
		// by `clearAskIfSettled`'s in-flight check instead.
		//
		// An entry bump was written first and removed: a mutation run showed it
		// survived removal, and reading it again there was nothing it could say
		// that these two do not already say between them. A second bump would
		// have been a second definition of "a resync happened" to keep in step
		// with the first.
		//
		// Only the STARTER reaches this: joined callers return the pending
		// promise above and never enter the try.
		bumpReconcileToken(ws);
	}
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
		// A fresh boot is the answer to a revocation banner — the Retry CTA is
		// `reset` + `bootstrap`. Cleared here rather than on success so the
		// banner goes away while the retry runs, and returns if it 403s again.
		if (!reentry) accessRevoked.delete(ws);
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
					? {
							items: [],
							cursor: state.cursor,
							includesUnparentedMetadata: state.includesUnparentedMetadata,
							accessEpoch: state.accessEpoch,
							// A reentry is not a read; the durable cache was
							// read on the bootstrap that set this state up.
							durableRead: state.cacheRead,
							retags: {},
						}
					: await persistHydrate(userId, ws);
				if (isStale()) return;
				// The durable cache has now answered, whatever it said. From
				// here an empty RAM state is evidence about the cache and not
				// merely about how far bootstrap has got.
				// `durableRead`, NOT an unconditional true: every failure inside
				// `hydrate` returns the same empty payload a genuinely empty
				// cache does, and treating a failed read as an empty cache is
				// how a silent adopt gets back in through the front door
				// (review round 2). A transient IDB failure now leaves the
				// baseline unacquired, which costs a resync and hides nothing.
				state.cacheRead = cached.durableRead;
				// A populated cache is one we've successfully synced
				// from before — either there are rows, or the cursor
				// has moved off the "0" floor (empty workspaces /
				// guests with item-level grants legitimately have
				// zero rows but a real cursor). Both deserve the
				// warm-path fast boot. Codex P2 round 8.
				const cacheIsPopulated =
					cached.includesUnparentedMetadata !== null &&
					(cached.items.length > 0 || cursorAsNum(cached.cursor) > 0);
				const hasCache = reentry || cacheIsPopulated;
				if (!reentry && cacheIsPopulated) {
					state.includesUnparentedMetadata = cached.includesUnparentedMetadata;
					// Adopt the PERSISTED epoch, not a fresh one — it is the
					// baseline the first reconcile compares against, and it is
					// the only record of what this cache was authorised for
					// when it was written. Overwriting it with the incoming
					// response's value is exactly the silent adopt that leaves
					// an offline revocation undetected.
					state.accessEpoch = cached.accessEpoch;
					for (const row of cached.items) {
						mergeRow(state, row);
					}
					if (cursorAsNum(cached.cursor) > cursorAsNum(state.cursor)) {
						state.cursor = cached.cursor;
					}
					// Apply renames recorded BEFORE these rows existed
					// (BUG-2601): hydrated rows carry the slug they were
					// cached under, and a rename touches no items, so no
					// delta below will ever re-stamp them. Applied then
					// cleared — a one-shot repair for the pre-hydration
					// window. Runs before the search rebuild so the
					// rebuilt index sees the live slugs. Intent recorded
					// under a DIFFERENT auth identity is discarded, not
					// applied (codex round 2 P2 — a cold state survives
					// the logout path, so this is the cross-user gate).
					// A null stamp = recorded before this session's auth
					// resolved — same session, so apply (codex round 7 P2).
					if (state.pendingRetagsUser === null || state.pendingRetagsUser === userId) {
						for (const [collectionId, newSlug] of state.pendingRetags) {
							applyRetag(ws, state, collectionId, newSlug);
						}
					}
					state.pendingRetags.clear();
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
						// ONE loop for both doors (TASK-2921). What used to live
						// here inline is `reconcileWorkspace`, which the
						// collection route's `deltaSync` now calls too. The
						// duplication had already drifted in four places, and the
						// comment that used to close this block named the cost:
						// a rule added at the public entry point silently did not
						// apply here. The ask is cleared inside, by whichever
						// iteration concluded catch-up.
						await reconcileWorkspace(ws, state, isStale);
						if (isStale()) return;
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
						if (isAuthError(err)) {
							// The whole reaction lives in
							// `dropCacheForAuthError` now, so the
							// route door gets it too (TASK-2921).
							dropCacheForAuthError(ws, state);
							throw err;
						}
						// Transient network failure. Cache stands and
						// state stays 'ready' so the UI keeps working.
						// `pendingResync` remains true so the next
						// bootstrap() call retries (Codex P2 round 5).
						// Permission revocation that doesn't change row
						// data used to be uncovered here — TASK-1360 and
						// DOC-1342 decision #3 punted it in favour of the
						// 403-on-click purge. IDEA-2898 covers it now, and
						// not from this branch: the caller's access
						// fingerprint rides on the delta response, and a
						// change routes through `ensureAccessScope` above
						// into an authoritative resync. What decision #3
						// still owns is the item READ — a 403 on click. What
						// it never reached is the LISTING, which is what
						// ItemPicker does with these rows.
						//
						// A `rate_limited` (429) error lands here too and is
						// intentionally treated as transient: the per-request
						// Retry-After backoff already ran centrally in
						// api/client.ts's request wrapper (TASK-2026), and a
						// thrown `rate_limited` breaks out of the reconcile loop
						// so we never hammer a busy server page-after-page. The
						// next bootstrap() retries the resync later.
					}
				} else {
					// Stage 2: cold path. /items-index full snapshot.
					const resp = await api.items.listIndex(ws, {
						includeArchived: true,
					});
					if (isStale()) return;
					state.includesUnparentedMetadata = resp.includes_unparented_metadata;
					if (resp.access_epoch !== undefined) {
						state.accessEpoch = resp.access_epoch;
					}
					for (const row of resp.items) {
						mergeRow(state, row);
					}
					if (cursorAsNum(resp.cursor) > cursorAsNum(state.cursor)) {
						state.cursor = resp.cursor;
					}
					state.bootstrapState = 'ready';
					// Cold path is a full snapshot — nothing pending.
					state.pendingResync = false;
					// Server rows carry the live collection slug — drop any
					// rename recorded pre-snapshot rather than re-applying
					// it over fresher server truth (BUG-2601).
					state.pendingRetags.clear();
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
					persistDelta(
						userId,
						ws,
						snapshot,
						state.cursor,
						resp.includes_unparented_metadata,
						// The baseline this snapshot established, so the durable
						// meta row records the scope its rows were fetched under
						// (IDEA-2898) — unless a refused snapshot has left the
						// durable rows describing an older scope, in which case
						// see durableEpochFor (TASK-2906).
						durableEpochFor(state),
					).catch(
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
	applyDelta(
		ws: string,
		changes: ItemChangeRow[],
		newCursor: string,
		includesUnparentedMetadata: boolean,
	): void {
		const state = ensureState(ws);
		state.includesUnparentedMetadata = includesUnparentedMetadata;
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
					change.seq < existing.seq &&
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
				if (existing?.seq !== undefined && change.seq === existing.seq && !change.moved_out) {
					const { deleted: _deleted, ...incoming } = change;
					const merged = mergeEqualSeqProjection(existing, incoming as ItemIndexRow);
					if (merged) {
						state.items.set(change.id, merged);
						localSearch.upsert(ws, merged);
						toPersist.push(merged);
						// Authoritative re-add — the row is visible again, so
						// lift any fence so its optimistic edits aren't dropped.
						state.fencedIds.delete(change.id);
						// Same for the eviction floor (TASK-2920). Dropping it is
						// housekeeping, not correctness: this row's seq is now at
						// or above the floor, so the floor could no longer refuse
						// anything the ordinary `existing.seq` guards admit.
						state.movedOutFloor.delete(change.id);
					} else {
						toPersist.push(existing);
					}
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
				// Record the seq at which this eviction was consumed, so a row
				// set fetched or read BEFORE it cannot reinstate the row after
				// it (TASK-2920). Highest wins: two evictions for one id can
				// arrive out of order across concurrent reconcile loops, and the
				// floor must not regress.
				if (change.seq !== undefined) {
					noteMovedOut(state, change.id, change.seq);
				}
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
			// Authoritative re-add under the current scope — lift any fence.
			state.fencedIds.delete(change.id);
			// ...and the eviction floor (TASK-2920), for the same reason: the
			// server has just said this row is visible again, at a seq at or
			// above the floor.
			state.movedOutFloor.delete(change.id);
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
		persistDelta(
			state.userId,
			ws,
			toPersist,
			newCursor,
			includesUnparentedMetadata,
			durableEpochFor(state),
			toRemove,
		).catch(
			() => undefined,
		);
	},

	/**
	 * Force a full snapshot when the caller's ACCESS set changes (IDEA-2898).
	 *
	 * The sibling of `ensureProjectionScope`, and deliberately the same shape:
	 * both answer "the server just told me my scope is not what this cache was
	 * built under". Since TASK-2921 there is ONE caller — `reconcileWorkspace`,
	 * which `bootstrap` and the layout-driven `localIndex.reconcile` both go
	 * through. Bootstrap used to carry the same comparison inline; it does not.
	 *
	 * Returns true when a resync ran, so the caller can skip applying a delta
	 * that the snapshot has already superseded.
	 */
	async ensureAccessScope(ws: string, accessEpoch: string | undefined): Promise<boolean> {
		// A server that does not send the field (an older build, mid-deploy)
		// must not be read as "the set changed". Absence is not a value.
		if (accessEpoch === undefined) return false;
		const state = ensureState(ws);
		if (state.accessEpoch === null) {
			// No baseline. If this cache holds anything, we cannot know what it
			// was authorised for, so the safe reading is that it may be stale —
			// resync rather than adopt. Mirrors ensureProjectionScope's null
			// branch exactly, including the "empty cache adopts silently" case,
			// which costs nothing because there is nothing to evict.
			if (state.items.size > 0 || cursorAsNum(state.cursor) > 0) {
				await resyncProjectionScope(ws, state, accessEpoch);
				// Same joined-resync case as below.
				if (state.accessEpoch === null) {
					state.accessEpoch = accessEpoch;
					// Only when the resync's snapshot actually landed durably —
					// see `durableSnapshotCommitted` (TASK-2906 round 2).
					if (state.durableSnapshotCommitted) {
						await persistAccessEpoch(state.userId, ws, accessEpoch, null);
					}
				}
				return true;
			}
			// AN EMPTY RAM STATE IS NOT AN EMPTY CACHE. This is the silent
			// adopt, and it is only safe once the durable cache has actually
			// answered: `bootstrap` awaits `hydrate` before it merges anything,
			// and an SSE-driven reconcile runs on its own subscription (the
			// workspace layout's, since TASK-2921) rather than behind that
			// await, so it can reach this line with RAM empty
			// and IDB holding rows from a scope nobody has checked. Adopting
			// there would stamp the new epoch onto the durable cache — via the
			// delta this caller is about to apply — and the stale row would then
			// hydrate under an epoch that agrees with the server forever.
			//
			// Declining to adopt is the safe direction: the delta persists with
			// a null epoch, and a null baseline over a populated cache resyncs
			// on the next reconcile.
			if (!state.cacheRead) return false;
			state.accessEpoch = accessEpoch;
			return false;
		}
		if (state.accessEpoch === accessEpoch) return false;
		// TERMINATION (round 1 F3). The resync adopts the snapshot's epoch when
		// it has one. When it does not — an older server answering the snapshot
		// while a newer one answers the delta — the baseline would be unchanged,
		// the very next poll would compare it against the same incoming epoch,
		// and it would resync again, forever. So the epoch we were TOLD goes in
		// as the fallback: a true statement about what the server last said,
		// which is all the baseline has ever claimed to be, and it lands in RAM
		// and IDB together.
		const beforeResync = state.accessEpoch;
		await resyncProjectionScope(ws, state, accessEpoch);
		// ...UNLESS THE RESYNC WAS JOINED, not started (round 3). Resyncs are
		// deduplicated per workspace, so a resync already in flight — started
		// by a projection mismatch, which passes no fallback — returns its
		// promise and this caller's fallback is never seen. The baseline is then
		// unchanged and the next poll asks again. Applying it here after the
		// await covers the joined case without a second resync; it is the same
		// statement ("the server last told us this"), made at the only point
		// where we can tell the fallback was dropped.
		if (state.accessEpoch === beforeResync) {
			state.accessEpoch = accessEpoch;
			// ...AND DURABLY, not only in RAM. The resync we JOINED ran its own
			// `persistReplace` with its own baseline, so the meta row still
			// records `beforeResync`; assigning here would leave RAM and disk
			// disagreeing, and the next reload would hydrate the old baseline
			// and resync again — every reload, for a scope that has not changed
			// since. The started-resync path has no such gap, because its
			// fallback is applied BEFORE the persist that happens inside it.
			// ...AND ONLY IF THAT RESYNC'S SNAPSHOT REACHED DISK (TASK-2906
			// round 2). The compare-and-set below is honest about the row it
			// patches but cannot see whether the `persistReplace` it is
			// finishing on behalf of committed. When that write was declined —
			// a cursor that lost the race to another tab, or a storage failure —
			// the durable rows still describe the OLD scope, and stamping the
			// new epoch over them would leave a cache that agrees with the
			// server about a scope it does not hold, with nothing left to
			// notice. Skipping keeps the stale baseline on disk, which is
			// exactly the disagreement that makes the next hydrate resync.
			if (state.durableSnapshotCommitted) {
				await persistAccessEpoch(state.userId, ws, accessEpoch, beforeResync);
			}
		}
		return true;
	},

	/** The access fingerprint this workspace's cache was last built under. */
	accessEpochFor(ws: string): string | null {
		return workspaces.get(ws)?.accessEpoch ?? null;
	},

	/** Force a full snapshot when the server's projection capability changes. */
	async ensureProjectionScope(ws: string, includesUnparentedMetadata: boolean): Promise<boolean> {
		const state = ensureState(ws);
		if (state.includesUnparentedMetadata === null) {
			if (state.items.size > 0 || cursorAsNum(state.cursor) > 0) {
				await resyncProjectionScope(ws, state);
				return true;
			}
			state.includesUnparentedMetadata = includesUnparentedMetadata;
			return false;
		}
		if (state.includesUnparentedMetadata === includesUnparentedMetadata) return false;
		await resyncProjectionScope(ws, state);
		return true;
	},

	/**
	 * Drive the workspace's reconcile loop (TASK-2921).
	 *
	 * The public door onto `reconcileWorkspace`, for callers that are not
	 * `bootstrap` — today the workspace layout, which drives it on every
	 * `sync_required` and every non-stale item event. Returns true if it caught
	 * up, false if it hit the page cap, so the caller can distinguish "current"
	 * from "gave up for now".
	 *
	 * A 401 / 403 runs `dropCacheForAuthError` HERE and then rethrows. An earlier
	 * draft of this comment said each door owned that reaction; that was true
	 * when the collection route was the only caller and stopped being true in
	 * the same unit, which is the whole hazard this sweep exists for.
	 *
	 * No-op returning true for an unhydrated workspace — there is nothing to
	 * reconcile and no ask outstanding, so reporting catch-up is honest.
	 */
	/**
	 * Was this workspace's cache dropped because ACCESS WAS REVOKED, as opposed
	 * to still loading or transiently failing? (TASK-2921.)
	 *
	 * Survives `reset()` deleting the workspace state, which is the whole point:
	 * the API client's global 403 handler resets before the error reaches any
	 * caller, so `bootstrapStateFor` reads `'cold'` at exactly the moment the UI
	 * needs to distinguish revoked from loading.
	 */
	accessRevokedFor(ws: string): boolean {
		return accessRevoked.has(ws);
	},

	async reconcile(ws: string): Promise<boolean> {
		const state = workspaces.get(ws);
		if (!state) return true;
		try {
			return await reconcileWorkspace(ws, state);
		} catch (err) {
			// 401 / 403 means the cached rows are no longer this caller's to
			// display, and the reaction is the STORE's (TASK-2921) — the same
			// `dropCacheForAuthError` bootstrap uses. It has to be here rather
			// than at the caller, because the caller is no longer the collection
			// route: the layout drives this now, for every route, and a purge
			// that only one page performed would have quietly stopped happening.
			// Rethrown either way so the caller's redirect handler still sees it.
			if (isAuthError(err)) dropCacheForAuthError(ws, state);
			throw err;
		}
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
	 *
	 * `sinceEpoch` (BUG-2098): pass the `scopeEpochFor(ws)` captured
	 * *before* issuing the create/update whose response you're upserting.
	 * If a projection resync bumped the epoch while that request was in
	 * flight, the response was authorized under the now-superseded scope
	 * and is refused. This closes the gap the fence alone can't cover: a
	 * brand-new create id was never in the map to be dropped, so it's
	 * never in `fencedIds` — without the epoch check a stale old-scope
	 * create would resurrect a row no new-scope delta will ever evict.
	 * Omit it (SSE / authoritative paths) to skip the check entirely.
	 */
	upsert(ws: string, row: ItemIndexRow | Item, sinceEpoch?: number): void {
		const state = ensureState(ws);
		let next = toSkinny(row);
		// Epoch guard (BUG-2098): a resync between request-issue and this
		// response means the row may belong to the old scope. Reject it —
		// the fence below only catches ids the resync explicitly dropped,
		// not a fresh create whose id was never in the map.
		if (sinceEpoch !== undefined && sinceEpoch < state.scopeEpoch) return;
		// Fence guard (Codex P1 round 8): a projection resync records the ids it
		// dropped as hidden under the new scope. This is the untrusted optimistic
		// path — a create/update response authorized under the OLD scope can
		// resolve here after the resync and would otherwise resurrect and persist
		// a now-hidden row that no new-scope delta will ever evict. Refuse it. The
		// fence lifts only via an authoritative applyDelta re-add or the next
		// resync recomputing the set.
		if (state.fencedIds.has(next.id)) return;
		// Eviction floor (TASK-2920). `fencedIds` does not cover this: it holds
		// ids a RESYNC dropped, and is REPLACED wholesale by the next resync,
		// whereas a `moved_out` eviction is recorded by a delta and must outlive
		// resyncs. The `sinceEpoch` guard above does not cover it either — that
		// bumps on a resync, and an eviction is not one.
		if (refusedByMovedOut(state, next)) return;
		const existing = state.items.get(row.id);
		next = preserveProjectionMetadata(existing, next);
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
	 * Re-stamp `collection_slug` on every cached row of a collection
	 * after a RENAME (BUG-2601). Rows are ingested with the slug the
	 * server denormalized at fetch time; a rename changes the slug
	 * without touching the items, so no `/items-changes` delta will
	 * ever re-ingest them — `getByCollection(newSlug)` returns [] and
	 * every rename-healed route (SSE and sync-pass alike) renders an
	 * empty board while the sidebar still counts the items.
	 *
	 * Matches by STABLE `collection_id` — never by old slug, which a
	 * different collection may have re-owned by the time we hear about
	 * the rename. Idempotent; rows already carrying `newSlug` are
	 * skipped. Mirrors the upsert write-through (search + IDB).
	 */
	retagCollection(ws: string, collectionId: string, newSlug: string, userId: string | null): void {
		// ensureState (not a bare get): a rename that lands before this
		// workspace ever hydrated must still be RECORDED, or the warm
		// hydrate restores rows under the dead slug with nothing left to
		// fix them (codex round 1 P1 — see pendingRetags on WorkspaceState).
		const state = ensureState(ws);
		// Ownership stamp (codex round 2 P2): intent recorded under a
		// DIFFERENT resolved auth identity is dropped, never mixed — a cold
		// state survives logout, and user A's rename intent must not steer
		// user B's warm cache. A null stamp means "recorded before this
		// session's auth resolved" and is ADOPTED by the first resolved
		// identity rather than discarded (codex round 7 P2): the SSE
		// connection that delivered the event was authenticated as this
		// session's user, so the intent is theirs.
		if (state.pendingRetagsUser !== userId) {
			if (state.pendingRetagsUser !== null) {
				state.pendingRetags.clear();
			}
			state.pendingRetagsUser = userId;
		}
		state.pendingRetags.set(collectionId, newSlug);
		applyRetag(ws, state, collectionId, newSlug);
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
	 * Whether the workspace's current projection includes the `is_unparented`
	 * bit — `true` for unrestricted callers, `false` for restricted ones,
	 * `null` when the workspace hasn't resolved a snapshot/delta yet
	 * (unknown). Consumers gating unparented-filter UI (TASK-2099 / PLAN-2095
	 * DR-2) must treat `null` the same as `false` — never show or apply the
	 * chip until this is confirmed `true`, so a restricted caller (or one
	 * whose scope hasn't resolved) never sees a side channel into structural
	 * parent state.
	 */
	includesUnparentedMetadataFor(ws: string): boolean | null {
		return workspaces.get(ws)?.includesUnparentedMetadata ?? null;
	},

	/**
	 * Whether a workspace's warm-cache snapshot has landed but the
	 * follow-up `/items-changes` reconcile hasn't confirmed it yet (see
	 * `bootstrap`'s warm-path doc comment) — OR a live-session resync
	 * (`resyncProjectionScope`, e.g. triggered mid-session by a permission
	 * change `deltaSync` detects) is still replaying post-snapshot
	 * mutations under the new scope. `includesUnparentedMetadataFor` can
	 * reflect a value that hasn't survived a full reconcile pass yet during
	 * either window.
	 *
	 * Consumers should use this ONLY to gate a DESTRUCTIVE decision that
	 * would be expensive/user-visible to get wrong in the "unknown, still
	 * resolving" direction (e.g. permanently discarding a URL/saved-view
	 * intent) — combine it with a CONFIRMED `includesUnparentedMetadataFor`
	 * value (`=== true` or `=== false`, not just "unavailable"), not used
	 * as a blanket gate on ordinary rendering/filtering. `bootstrap()`'s
	 * own reconcile loop clears this on catch-up for the cold/warm-boot
	 * path; a caller with an independent reconcile loop (e.g. the
	 * collection page's SSE/periodic-sync-driven `deltaSync`) MUST call
	 * `markCaughtUp` itself on ITS catch-up, or gating routine availability
	 * on this being `false` can wedge a UI element hidden indefinitely
	 * after a live permission upgrade (Codex review round 3 P1/P2, round 4
	 * — TASK-2099 / PLAN-2095 DR-2). Returns `false` for an unhydrated
	 * workspace (nothing pending because nothing has started).
	 */
	pendingResyncFor(ws: string): boolean {
		return workspaces.get(ws)?.pendingResync ?? false;
	},

	/**
	 * Mark a workspace's resync as caught up — clears `pendingResync`.
	 *
	 * `bootstrap()`'s own internal reconcile loop already does this
	 * (`if (caughtUp) state.pendingResync = false`), but that loop only
	 * runs for the initial cold/warm-boot path. A resync triggered mid-
	 * session (`resyncProjectionScope`, e.g. because the collection page's
	 * OWN `deltaSync` loop — driven by SSE events / periodic sync, not
	 * `bootstrap()` — detected a projection-scope change) has no such
	 * owner: nothing else ever flips `pendingResync` back to `false` for
	 * it, so it would stay stuck `true` for the rest of the session. That
	 * wedges any consumer gating a destructive decision on
	 * `pendingResyncFor` (TASK-2099 / PLAN-2095 DR-2's
	 * `unparentedConfirmedRestricted`, Codex review round 4) — a caller
	 * downgraded and later re-upgraded within the same session would never
	 * see the "confirmed restricted" transition fire, so a stuck
	 * unparented-filter intent from before the downgrade would silently
	 * reactivate on the upgrade instead of staying cleared.
	 *
	 * Callers should invoke this once THEIR OWN independent reconcile loop
	 * confirms it has caught up (the same predicate `bootstrap()` uses:
	 * an empty delta batch, or the response cursor not advancing past
	 * `since`) — mirroring exactly what `bootstrap()`'s loop already does
	 * for the path it owns.
	 *
	 * The argument MUST be the `reconcileTokenFor(ws)` the caller captured when
	 * it ISSUED the request it is reporting on — not one read at (or
	 * reconfirmed immediately before) the point it decided "caught up", because
	 * the staleness this guards against is decided at request time (TASK-2909) —
	 * the same value already threaded through these reconcile loops as
	 * `epochBefore`. SSE, periodic sync, and `bootstrap()` can all trigger
	 * reconciliations concurrently; without this check, one caller's stale
	 * catch-up confirmation could unconditionally clear `pendingResync`
	 * out from under a DIFFERENT, still-in-progress (or just-restarted)
	 * resync that landed after this caller's own epoch check — silencing
	 * `bootstrap()`'s retry of a resync/replay that hasn't actually
	 * finished (Codex review round 5). A mismatched epoch means a resync
	 * has landed since this caller confirmed catch-up, so the clear is
	 * skipped — whichever loop eventually catches up under the NEW epoch
	 * will clear it.
	 *
	 * No-op for an unhydrated workspace.
	 */
	markCaughtUp(ws: string, token: number): void {
		const state = workspaces.get(ws);
		if (!state) return;
		// WHAT THE TOKEN MUST BE, AND WHAT IT IS NOT (TASK-2909).
		//
		// `token` is the caller's `reconcileTokenFor(ws)` captured when it
		// ISSUED the request it is now reporting on. Not a scope epoch — those
		// fence optimistic WRITES and say nothing about resync overlap — and not
		// a value read at the moment of reporting, because the staleness this
		// guards against is decided at request time, not at clear time.
		//
		// `clearAskIfSettled` compares it against the module-level reconcile
		// token, which bumps when a resync SETTLES and when the workspace's rows
		// are DROPPED, and additionally refuses while a resync is outstanding.
		// Between them they cover both ways a response can be overtaken: one
		// that returns while the resync is still running (the counter has not
		// moved yet — the in-flight check catches it) and one that returns after
		// it settled (nothing is in flight — the bumped counter catches it).
		// Both were computed against the PRE-snapshot cursor, so once the resync
		// pins the cursor their verdict is not merely early, it is stale. That is
		// also why the refusal is not deferred and re-applied: applying it later
		// would apply a verdict the snapshot invalidated. The caller's next
		// trigger re-polls from the pinned cursor and clears honestly.
		//
		// DISK IS NOT A CONDITION, deliberately (review round 1). An earlier
		// version also refused while `durableSnapshotCommitted` was false. That
		// buys no repair and costs a wedge: `pendingResyncFor` gates a
		// destructive decision (TASK-2099 / PLAN-2095 DR-2), and a persistently
		// refused replace or a failing IndexedDB `open()` would make the flag
		// un-clearable for the session. The refused case is repaired through a
		// channel that does not touch this flag — `durableEpochFor` stops
		// vouching for the epoch, so the durable baseline stops agreeing with
		// the server and the next hydrate resyncs. PLAN-2903 item 3's "nothing
		// retries" was true when written and stopped being true at TASK-2906;
		// the premise correction is on that plan's trail.
		clearAskIfSettled(ws, state, token);
	},

	/**
	 * Current projection-scope epoch — bumped when a resync STARTS installing a
	 * new authoritative snapshot. Captured by a caller issuing an optimistic
	 * MUTATION and handed back to `upsert`, which refuses a write authorised
	 * under a superseded scope. Returns 0 for an unhydrated workspace.
	 *
	 * Reconcile loops want `reconcileTokenFor` instead (TASK-2909): this value
	 * cannot say whether a resync overlapped a request, and giving it that
	 * second meaning broke the write fence.
	 */
	/**
	 * The token a reconcile loop captures when it starts an `/items-changes`
	 * request and hands back to `markCaughtUp` (TASK-2909).
	 *
	 * Distinct from `scopeEpochFor`, which answers "was my optimistic write
	 * authorised under the current scope" and must NOT change when a resync
	 * merely finishes persisting — a settle bump there rejected user writes
	 * issued after the new snapshot was already installed (review round 3).
	 * This one changes when a resync SETTLES, which is what makes a capture
	 * taken before or during that resync detectably stale afterwards; the
	 * mid-flight case is covered by the in-flight check in `clearAskIfSettled`
	 * rather than by this counter.
	 *
	 * Returns 0 for a workspace nothing has happened to yet — but NOT as an
	 * "unhydrated" sentinel, and no caller may read it as one: the counter
	 * outlives the state, so a slug reset while unhydrated answers 1 with no
	 * state in existence (IDEA-2913 review). The only meaningful use is
	 * comparing a captured value against a later one.
	 */
	reconcileTokenFor(ws: string): number {
		return reconcileTokens.get(ws) ?? 0;
	},

	scopeEpochFor(ws: string): number {
		return workspaces.get(ws)?.scopeEpoch ?? 0;
	},

	/**
	 * How many times this workspace's state has been DROPPED — sign-out, a
	 * 403 membership purge, a workspace deletion.
	 *
	 * Deliberately kept OUTSIDE `workspaces`, unlike `scopeEpoch` and the
	 * internal `generation`, because `reset()` deletes that entry: a counter
	 * living on the state object restarts at 0 with the replacement, so
	 * comparing it across a reset compares two different identities that
	 * happen to agree. Those two are safe only because their readers hold a
	 * REFERENCE to the state object; a caller outside this module cannot.
	 *
	 * For such a caller — one holding an in-flight request issued against this
	 * workspace — capturing this before the request and requiring it unchanged
	 * afterwards is what distinguishes "still the state I was authorized
	 * against" from "a fresh state that has coincidentally reached the same
	 * numbers". `reconcileTokenFor` answers the resync-overlap question and this
	 * answers the identity one; a write-back needs both (TASK-2877). Still true
	 * after TASK-2909, and sharper: a reset between a reconcile loop's capture
	 * and its response gives the REPLACEMENT state a token starting from zero,
	 * so the token alone cannot tell a stale response from a fresh one across a
	 * reset. That gap is older than the token (the scope epoch it replaced had
	 * it too) and is filed rather than fixed here.
	 */
	resetGenerationFor(ws: string): number {
		return resetGenerations.get(ws) ?? 0;
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
		// Bumped for EVERY reset, including one for a workspace with no state
		// yet: a caller's captured value has to change even when the drop found
		// nothing to drop, or a purge racing a first bootstrap reads as no
		// purge at all.
		markWorkspaceDropped(ws);
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
		projectionResyncs.delete(ws);

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
