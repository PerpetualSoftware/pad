// localIndexPersistence — IndexedDB write-behind layer for localIndex
// (PLAN-1343 / TASK-1356). Per DOC-1342 design decision #4: the
// in-RAM Svelte store is canonical; IDB is a hydration source on
// cold/warm boot and a write-behind cache for every mutation. The
// reader path is unaffected — consumers always go through localIndex,
// never through this module directly.
//
// Database shape: one IDB database per (user, workspace) pair, named
// `pad-local-index-{userId}-{wsSlug}`. Scoping by user is required —
// the cache is what the user could see last sync, and if a different
// user signs into the same browser, exposing the previous user's
// view would be a real correctness/permission leak. Anonymous /
// bootstrap-time callers (with no user id yet) use the `anon`
// namespace; those caches are independent of any signed-in user.
//
// Two object stores:
//
//   items  (keyPath: 'id')   — ItemIndexRow rows, keyed by item.id
//   meta   (keyPath: 'key')  — { key: 'sync', cursor, schemaVersion }
//
// SCHEMA_VERSION is the local equivalent of the Yjs schemaVersion in
// `web/src/lib/collab/schemaVersion.ts`. Bump it whenever the
// `ItemIndexRow` wire shape or store layout changes incompatibly —
// hydrators will see the mismatch on open and drop the persisted
// data, forcing a full /items-index resync. The server is the
// source of truth, so dropping the cache is always safe.
//
// All public functions never throw. Storage failures (Safari private
// mode, browser eviction, quota exceeded) degrade silently to
// in-memory only operation; the next bootstrap hits /items-index
// the normal way and the warm-load fast path simply skips.
//
// SSR-safe: every IDB call is gated on `typeof indexedDB !== 'undefined'`
// so SvelteKit's prerender / SSR phase doesn't blow up.

import { openDB, type IDBPDatabase } from 'idb';
import type { ItemIndexRow } from '$lib/types';
import { resolveRowWrite, type Tombstone } from './itemRowMerge';

/**
 * IDB FORMAT version — the argument to `openDB`, used by the library to drive
 * store-creation migrations. Bumped 1 → 2 in PLAN-2636 unit 2 to add the
 * `tombstones` object store (BUG-2633). Distinct from
 * `LOCAL_INDEX_SCHEMA_VERSION`, which versions the ROW SHAPE: the row shape did
 * not change, so that constant stays 3. A v1 DB reopening under v2 gets the
 * upgrade callback with `oldVersion === 1`; `items`/`meta` data is RETAINED and
 * only the new store is created (empty — old exposure until first resync, not
 * corruption). An OLD build reopening a v2 DB at version 1 gets a VersionError,
 * which `open()` catches → returns null → that tab degrades to memory-only.
 * Fail-safe and transient during a deploy.
 *
 * Exported so the test harness opens second connections / raw readers at the
 * SAME format version the module creates — a drift there would VersionError
 * every raw read once the module owns a higher-versioned DB.
 */
export const IDB_FORMAT_VERSION = 2;

/**
 * SCHEMA_VERSION is the cache-shape contract. Bump it whenever the
 * `ItemIndexRow` skinny projection or this module's IDB layout changes
 * incompatibly — old clients reopening on a new build see the
 * mismatch and wipe their store, then re-bootstrap from
 * `/items-index`. Server truth (items.content) is never persisted
 * here, so a cache wipe loses nothing.
 */
export const LOCAL_INDEX_SCHEMA_VERSION = 3;

/** Result of a `hydrate()` call. Empty payload when there's no cache yet. */
export interface HydrateResult {
	items: ItemIndexRow[];
	cursor: string;
	includesUnparentedMetadata: boolean | null;
	/**
	 * The durable retag overlay applied to `items` before return (BUG-2634):
	 * `collection_id → newSlug`. Returned for inspection; `items` already
	 * reflect it, so the warm-boot caller needs no further action. Empty when
	 * no rename has been persisted. See `persistRetag`.
	 */
	retags: Record<string, string>;
}

/** Shape of the sync row stored in the `meta` store (keyed by `key: 'sync'`). */
interface MetaRow {
	key: 'sync';
	cursor: string;
	schemaVersion: number;
	includesUnparentedMetadata: boolean;
}

/**
 * The durable retag overlay row in the `meta` store (BUG-2634), keyed by
 * `key: 'retags'`. `map` is `collection_id → latest new slug`; last rename per
 * collection wins, mirroring RAM's `pendingRetags`. Written in the same tx as
 * the row rewrites by `persistRetag`, applied at read time by `hydrate`, and
 * dropped by `persistReplace` (authoritative snapshot supersedes).
 */
interface RetagsRow {
	key: 'retags';
	map: Record<string, string>;
}

/**
 * A hard-delete tombstone row in the `tombstones` store (BUG-2633). Structural
 * alias of `Tombstone` from itemRowMerge — kept as the persisted shape's name
 * at the store boundary.
 */
type TombstoneRow = Tombstone;

// Open IDB connections are cached per (user, workspace) pair. The
// map key matches `dbName()` so cache slots can't collide across
// user namespaces.
const dbs = new Map<string, IDBPDatabase>();

function isSupported(): boolean {
	return typeof indexedDB !== 'undefined';
}

/**
 * Encode a value so it's safe to embed in an IDB database name.
 * Names allow any UTF-16 string per spec, but URL-encoding keeps
 * the on-disk handle predictable in dev tools and avoids surprises
 * from exotic IDs.
 */
function safe(s: string): string {
	return encodeURIComponent(s);
}

function dbName(userId: string | null, ws: string): string {
	const ns = userId ? safe(userId) : 'anon';
	return `pad-local-index-${ns}-${safe(ws)}`;
}

function key(userId: string | null, ws: string): string {
	return dbName(userId, ws);
}

/**
 * Decode a cursor string to its numeric `seq` for tombstone stamping. Cursors
 * are decimal-encoded seq values; a non-numeric / empty cursor decodes to 0,
 * matching localIndex's `cursorAsNum`. A tombstone stamped 0 is the weakest
 * possible ordering evidence — it refuses only seq-less and seq-0 resurrections
 * — which is the correct floor when no real cursor is in hand.
 */
function seqFromCursor(cursor: string): number {
	const n = Number(cursor);
	return Number.isFinite(n) ? n : 0;
}

/**
 * Write a tombstone for `id` at `deletedAtSeq`, but NEVER lower an existing
 * tombstone's stamp (codex F4). Tombstone puts are otherwise last-write-wins,
 * so an out-of-order cross-tab eviction (a lower-cursor delta arriving after a
 * higher one) could weaken the gate and let a stale row between the two stamps
 * resurrect. Keeping the max makes the stamp monotonic. Best-effort like every
 * other write here — a failed put is swallowed so `tx.done` still resolves.
 */
async function raiseTombstone(
	store: { get(key: string): Promise<unknown>; put(value: TombstoneRow): Promise<unknown> },
	id: string,
	deletedAtSeq: number,
): Promise<void> {
	const prior = (await store.get(id)) as TombstoneRow | undefined;
	const seq = prior ? Math.max(prior.deletedAtSeq, deletedAtSeq) : deletedAtSeq;
	store.put({ id, deletedAtSeq: seq } satisfies TombstoneRow).catch(() => undefined);
}

/**
 * Open the workspace's IDB database for the given user, creating
 * object stores on first run. Cached so subsequent calls reuse the
 * connection. Returns null on any storage failure — callers must
 * treat that as "no cache available, fall back to network".
 */
async function open(
	userId: string | null,
	ws: string,
): Promise<IDBPDatabase | null> {
	if (!isSupported()) return null;
	const k = key(userId, ws);
	const cached = dbs.get(k);
	if (cached) return cached;

	try {
		// The version arg to openDB is the IDB-FORMAT version, used by the
		// library to drive store-creation migrations. It is now 2 (was 1) to
		// add the `tombstones` store — see IDB_FORMAT_VERSION. Content-shape
		// versioning stays decoupled in the `schemaVersion` row in `meta`
		// (LOCAL_INDEX_SCHEMA_VERSION), so a row-shape bump and a format bump
		// remain independent. The upgrade callback is idempotent per store, so
		// it serves both a fresh v2 create (all three stores) and a v1 → v2
		// upgrade (only `tombstones` is missing; `items`/`meta` are retained).
		const db = await openDB(dbName(userId, ws), IDB_FORMAT_VERSION, {
			upgrade(db) {
				if (!db.objectStoreNames.contains('items')) {
					db.createObjectStore('items', { keyPath: 'id' });
				}
				if (!db.objectStoreNames.contains('meta')) {
					db.createObjectStore('meta', { keyPath: 'key' });
				}
				if (!db.objectStoreNames.contains('tombstones')) {
					db.createObjectStore('tombstones', { keyPath: 'id' });
				}
			},
		});
		dbs.set(k, db);
		return db;
	} catch {
		return null;
	}
}

/**
 * Read everything from IDB for a workspace. Returns the persisted
 * items + cursor, OR an empty result when:
 *   - IDB isn't supported (SSR, ancient browser),
 *   - the persisted schemaVersion doesn't match LOCAL_INDEX_SCHEMA_VERSION
 *     (in which case the store is also wiped as a side effect so the
 *     next persist write starts fresh),
 *   - opening or reading fails (storage error, quota issue).
 *
 * Never throws. The caller does the cold-path /items-index fetch when
 * `items` is empty.
 */
export async function hydrate(
	userId: string | null,
	ws: string,
): Promise<HydrateResult> {
	const empty: HydrateResult = {
		items: [],
		cursor: '0',
		includesUnparentedMetadata: null,
		retags: {},
	};
	if (!isSupported()) return empty;

	const db = await open(userId, ws);
	if (!db) return empty;

	try {
		const tx = db.transaction(['items', 'meta'], 'readonly');
		const metaStore = tx.objectStore('meta');
		const meta = (await metaStore.get('sync')) as MetaRow | undefined;

		// Schema-version mismatch is the "your local cache is from a
		// previous incompatible build" case. Drop everything and
		// signal an empty cache. The next bootstrap fully resyncs.
		if (meta && meta.schemaVersion !== LOCAL_INDEX_SCHEMA_VERSION) {
			await tx.done.catch(() => undefined);
			await wipe(userId, ws);
			return empty;
		}

		const retagsRow = (await metaStore.get('retags')) as RetagsRow | undefined;
		const retags = retagsRow?.map ?? {};
		const rawRows = (await tx.objectStore('items').getAll()) as ItemIndexRow[];
		await tx.done.catch(() => undefined);

		// Apply the durable retag overlay (BUG-2634) AFTER the read: a collection
		// rename touches no items, so no delta re-stamps a persisted row cached
		// under the dead slug, and RAM's `pendingRetags` repair is in-memory /
		// single-session. Reapplying the persisted intent on every hydrate is what
		// makes a rename survive a reload. Membership + already-current checks
		// mirror `shouldApplyRetag`: only rows whose `collection_id` matches the
		// renamed collection are touched, and a row already carrying the new slug
		// is left alone (also skips a row that has since MOVED to another
		// collection — its `collection_id` no longer matches).
		//
		// RESIDUAL (codex F3, lead-accepted option (a)). This ALWAYS trusts the
		// recorded rename, so it can revert a NEWER authoritative slug. TRIGGER: a
		// tab records rename coll-a v0->v1 (overlay {coll-a:v1}), MISSES the later
		// v1->v2 rename SSE (reconnect gap), then receives an item-delta carrying
		// collection_slug=v2 — hydrate rewrites v2 back to v1. There is no LOCAL
		// fix: `collection_slug` is out-of-band, renames carry no item seq, so the
		// 2634 racing OLD-slug delta and this F3 NEWER-slug delta are
		// indistinguishable (both arrive with seq > the rename cursor, slug !=
		// overlay). Clearing the overlay on a divergent-slug delta only flips which
		// case loses — it sacrifices the common 2634 race to protect the missed-SSE
		// rarity. HEAL PATHS: (1) the next rename event for coll-a overwrites the
		// overlay (latest-wins); (2) the BUG-2601 SSE-heal / sync-pass reconcile
		// re-stamps the live slug; (3) any full resync -> persistReplace drops the
		// overlay. The true disambiguator is server-side collection-slug versioning
		// (not built here). Net strictly better than main, where the rename is lost
		// across every reload.
		const items = (rawRows ?? []).map((row) => {
			const newSlug = row.collection_id ? retags[row.collection_id] : undefined;
			if (newSlug !== undefined && row.collection_slug !== newSlug) {
				return { ...row, collection_slug: newSlug };
			}
			return row;
		});

		return {
			items,
			cursor: meta?.cursor ?? '0',
			includesUnparentedMetadata: meta?.includesUnparentedMetadata ?? null,
			retags,
		};
	} catch {
		return empty;
	}
}

/**
 * WHY THE WRITE POLICY EXISTS (the race resolveRowWrite arbitrates). `upsert`
 * hands persistUpserts a snapshot taken from RAM and does not await it. An SSE
 * delta for the same row can commit its own atomic rows+cursor transaction in
 * between, after which the older snapshot lands LAST and would leave IDB
 * holding a pre-delta row while the persisted cursor sits past that delta —
 * warm boot then hydrates the stale row and `/items-changes?since=cursor` never
 * returns it again (BUG-2609). RAM is unaffected (single-threaded and
 * seq-guarded); only the warm-boot cache can regress. The read-modify-write is
 * done INSIDE the transaction so the decision compares against committed state
 * at write time, not snapshot time — IDB serializes overlapping readwrite
 * transactions on a store, so the `get` always sees a competing tx's committed
 * `put`.
 *
 * The decision itself — seq guard + no-seq asymmetry (BUG-2609), tombstone gate
 * (BUG-2633), equal-seq projection MERGE (BUG-2635), and projection preserve on
 * write — is the pure `resolveRowWrite` in `itemRowMerge.ts`, shared with RAM's
 * `mergeRow`. This module owns only the transaction and the tombstone delete on
 * supersession.
 */

/**
 * Upsert a batch of rows in a single transaction. Used by
 * `applyDelta`/`bootstrap` write-through. Batching matters when an
 * SSE flurry arrives — a single tx is much cheaper than N
 * one-shot puts.
 */
export async function persistUpserts(
	userId: string | null,
	ws: string,
	rows: ItemIndexRow[],
): Promise<void> {
	if (!isSupported() || rows.length === 0) return;
	const db = await open(userId, ws);
	if (!db) return;
	try {
		const tx = db.transaction(['items', 'tombstones'], 'readwrite');
		const itemsStore = tx.objectStore('items');
		const tombstones = tx.objectStore('tombstones');
		for (const row of rows) {
			const stored = (await itemsStore.get(row.id)) as ItemIndexRow | undefined;
			const tombstone = (await tombstones.get(row.id)) as TombstoneRow | undefined;
			const decision = resolveRowWrite(stored, tombstone, row);
			if (decision.action === 'skip') continue;
			itemsStore.put(decision.row).catch(() => undefined);
			// The write is authoritative supersession of a hard delete (rule 1):
			// clear the tombstone in the SAME tx so it can't re-refuse a later
			// legitimate write for this id.
			if (tombstone) tombstones.delete(row.id).catch(() => undefined);
		}
		await tx.done;
	} catch {
		/* swallow — best-effort cache */
	}
}

/**
 * Persist a collection RENAME by rewriting `collection_slug` on the given rows
 * IN PLACE, rather than putting whole snapshots (BUG-2609).
 *
 * A retag is a field-level intent — "these rows are in a collection that got
 * renamed" — and expressing it as a whole-row put makes it two claims at once,
 * the second of which is false: it also asserts every other field still looks
 * like the snapshot taken from RAM. That second claim is what the seq guard in
 * `resolveRowWrite` has to refuse when a newer delta has landed, and refusing
 * it drops the rename with it.
 *
 * Dropping the rename is NOT self-healing, which is the reason this function
 * exists instead of an exemption from the guard. `applyRetag` does not bump
 * seq, a collection rename touches no items so no item delta ever re-stamps
 * them, and localIndex's `pendingRetags` repair is explicitly in-memory and
 * scoped to a single session's pre-hydration window. A persisted slug that
 * loses its rename therefore stays wrong across reloads with nothing left to
 * correct it.
 *
 * Reading each row inside the transaction and changing only the one field
 * keeps both properties: the newer row's fields survive, and the rename lands.
 * Rows absent from the cache are skipped — there is nothing to rename, and
 * inserting a snapshot here would resurrect rows a delta may have removed.
 * Rows that have MOVED to another collection are skipped too; see
 * shouldApplyRetag.
 *
 * DURABLE OVERLAY (BUG-2634, PLAN-2636 unit 2). Rewriting the rows in place is
 * still racy in the other direction: a delta captured BEFORE the rename can
 * commit AFTER it and whole-row put an older `collection_slug` at a newer seq,
 * which no seq compare can arbitrate (the slug is out-of-band relative to the
 * item's version). And a lost write (tx abort) drops the rename with nothing to
 * retry it. Both are closed by ALSO persisting the retag INTENT: `persistRetag`
 * upserts `{key:'retags', map:{[collectionId]: newSlug}}` in the `meta` store,
 * in the SAME tx as the row rewrites, and `hydrate` reapplies the overlay to
 * every matching row after the read. A racing older-slug delta is then
 * corrected on the next hydrate rather than surviving. Latest rename per
 * collection wins (read-modify-write the map). `persistReplace` drops the key —
 * an authoritative snapshot already carries the live slug (BUG-2601), so a
 * stale overlay reapplied over it could regress a newer rename.
 */
export function shouldApplyRetag(
	existing: ItemIndexRow | undefined,
	collectionId: string,
	newSlug: string,
): boolean {
	if (!existing) return false;
	// Membership is re-checked HERE, not trusted from the snapshot. A row can
	// move to another collection between the RAM retag and this transaction,
	// and applying the renamed collection's slug to it would persist a row
	// whose collection_id and collection_slug disagree — behind the cursor,
	// so no delta repairs it (codex round 5).
	if (existing.collection_id !== collectionId) return false;
	if (existing.collection_slug === newSlug) return false;
	return true;
}

export async function persistRetag(
	userId: string | null,
	ws: string,
	collectionId: string,
	ids: string[],
	newSlug: string,
): Promise<void> {
	if (!isSupported() || ids.length === 0) return;
	const db = await open(userId, ws);
	if (!db) return;
	try {
		const tx = db.transaction(['items', 'meta'], 'readwrite');
		const store = tx.objectStore('items');
		for (const id of ids) {
			const existing = (await store.get(id)) as ItemIndexRow | undefined;
			if (!shouldApplyRetag(existing, collectionId, newSlug)) continue;
			store.put({ ...existing!, collection_slug: newSlug }).catch(() => undefined);
		}
		// Persist the retag INTENT (BUG-2634) in the same tx: read-modify-write
		// the `retags` overlay map so hydrate can reapply the rename to rows a
		// racing older-slug delta or a lost in-place write left wrong. Latest
		// rename per collection wins.
		const metaStore = tx.objectStore('meta');
		const existingOverlay = (await metaStore.get('retags')) as RetagsRow | undefined;
		const map = { ...(existingOverlay?.map ?? {}), [collectionId]: newSlug };
		metaStore.put({ key: 'retags', map } satisfies RetagsRow).catch(() => undefined);
		await tx.done;
	} catch {
		/* swallow — best-effort cache */
	}
}

/**
 * Atomically advance a delta — write upserted rows AND the new
 * cursor in a single IDB transaction. If the tx fails or is aborted
 * (browser eviction, quota, tab freeze), nothing is written so the
 * persisted cursor never overshoots the persisted rows. The next
 * warm hydrate sees a consistent floor and `/items-changes?since=`
 * can pick up from there without skipping rows. Codex P2 (round 1)
 * caught the divergence risk of separate row/cursor writes.
 *
 * Soft deletes flow through `rows` (as upserts with `deleted_at`
 * populated). Hard removals are either passed as `removeIds` (so a
 * moved-out eviction lands in the SAME tx as the cursor advance and
 * can't resurrect on warm boot — BUG-1675) or, for paths without a
 * cursor advance, go through `persistRemovals`. Each hard removal also
 * writes a tombstone stamped with this delta's cursor so a delayed stale
 * snapshot can't reinsert the id behind the cursor (BUG-2633).
 */
export async function persistDelta(
	userId: string | null,
	ws: string,
	rows: ItemIndexRow[],
	cursor: string,
	includesUnparentedMetadata: boolean,
	removeIds: string[] = [],
): Promise<void> {
	if (!isSupported()) return;
	const db = await open(userId, ws);
	if (!db) return;
	try {
		const tx = db.transaction(['items', 'meta', 'tombstones'], 'readwrite');
		const itemsStore = tx.objectStore('items');
		const tombstones = tx.objectStore('tombstones');
		// Same policy as persistUpserts (resolveRowWrite): the race runs in both
		// directions, so a delta must not overwrite a row that is already NEWER
		// in the cache, and a tombstone must refuse a stale resurrection.
		// Skipping a row here is safe with the cursor advance below — a newer
		// stored row already reflects state past this delta.
		for (const row of rows) {
			const stored = (await itemsStore.get(row.id)) as ItemIndexRow | undefined;
			const tombstone = (await tombstones.get(row.id)) as TombstoneRow | undefined;
			const decision = resolveRowWrite(stored, tombstone, row);
			if (decision.action === 'skip') continue;
			itemsStore.put(decision.row).catch(() => undefined);
			if (tombstone) tombstones.delete(row.id).catch(() => undefined);
		}
		// Hard removals (moved-out evictions — BUG-1675) land a TOMBSTONE stamped
		// with this delta's cursor in the SAME tx (BUG-2633). The resurrect
		// window is exactly "behind the persisted cursor", so the cursor is the
		// right stamp: a delayed snapshot for this id at seq <= cursor is stale
		// and resolveRowWrite refuses it; a genuinely newer one (seq > cursor)
		// supersedes and clears the tombstone.
		const deletedAtSeq = seqFromCursor(cursor);
		for (const id of removeIds) {
			itemsStore.delete(id).catch(() => undefined);
			await raiseTombstone(tombstones, id, deletedAtSeq);
		}
		tx.objectStore('meta')
			.put({
				key: 'sync',
				cursor,
				schemaVersion: LOCAL_INDEX_SCHEMA_VERSION,
				includesUnparentedMetadata,
			} satisfies MetaRow)
			.catch(() => undefined);
		await tx.done;
	} catch {
		/* swallow — best-effort cache */
	}
}

/**
 * Atomically REPLACE the persisted snapshot — clear the items store and
 * write the given rows + cursor in a single readwrite transaction. Used by
 * the projection-scope resync, which needs the persisted cache to exactly
 * mirror an authoritative re-fetch (rows dropped by a permission downgrade
 * must not survive) without the cross-tab hazard of `wipe()`: a
 * `deleteDatabase()` resolves on `onblocked` while another tab holds the DB
 * open, leaving the delete pending so a following reopen+write can queue
 * behind it indefinitely. A single transaction over the still-open
 * connection sidesteps that — the clear and the puts commit together (or not
 * at all), and no connection is ever torn down.
 *
 * RESIDUAL (codex F2, lead-accepted). Clearing `tombstones` for ids the
 * snapshot omits reopens, for those ids, the cross-tab resurrection window a
 * tombstone otherwise closes: a stale `persistUpserts` from ANOTHER tab — one
 * that did not run this resync, so its RAM `fencedIds` does not cover the id —
 * can land after the clear and reinsert an omitted row behind the replacement
 * cursor. This is PRE-EXISTING to the item-clear below: before unit 2 the same
 * tab could resurrect the same row with no tombstone in the picture at all, so
 * tombstones only ever ADDED protection and this clear does not widen the
 * hazard. Within one tab the `fencedIds` guard refuses the stale upsert before
 * it reaches persistence. Self-healing: the next resync recomputes the fence
 * and re-drops the row. Not worth a key-diff in this tx to close.
 */
export async function persistReplace(
	userId: string | null,
	ws: string,
	rows: ItemIndexRow[],
	cursor: string,
	includesUnparentedMetadata: boolean,
): Promise<void> {
	if (!isSupported()) return;
	const db = await open(userId, ws);
	if (!db) return;
	try {
		const tx = db.transaction(['items', 'meta', 'tombstones'], 'readwrite');
		const itemsStore = tx.objectStore('items');
		// Queued before the puts; IDB executes requests against a store in
		// issue order, so the clear always lands first.
		itemsStore.clear().catch(() => undefined);
		// The authoritative snapshot supersedes every tombstone and every
		// pending rename: its rows ARE the current truth under the (possibly
		// downgraded) scope, and its slugs are already live (BUG-2601). Drop
		// both overlays so a stale tombstone can't refuse a legitimate row and a
		// stale retag can't regress a newer slug on the next hydrate.
		tx.objectStore('tombstones').clear().catch(() => undefined);
		const metaStore = tx.objectStore('meta');
		metaStore.delete('retags').catch(() => undefined);
		for (const row of rows) {
			itemsStore.put(row).catch(() => undefined);
		}
		metaStore
			.put({
				key: 'sync',
				cursor,
				schemaVersion: LOCAL_INDEX_SCHEMA_VERSION,
				includesUnparentedMetadata,
			} satisfies MetaRow)
			.catch(() => undefined);
		await tx.done;
	} catch {
		/* swallow — best-effort cache */
	}
}

/**
 * Delete rows by id (hard remove). Used by `localIndex.remove` for
 * 403 purge (TASK-1360) and any other hard-delete path. Soft deletes
 * stay in the cache as upserts with `deleted_at` populated — they
 * flow through `persistUpserts`.
 *
 * Like `persistDelta`'s `removeIds`, a removal lands a TOMBSTONE so a delayed
 * snapshot can't resurrect the id (BUG-2633). This path has no cursor advance
 * in hand, so it stamps the strongest floor available: the PERSISTED cursor
 * (`meta.sync`) when a sync has happened, else the removed row's OWN seq (codex
 * F1 — an `upsert` can persist a row before any sync writes meta.sync, and that
 * row must still be un-resurrectable after removal), else 0. `raiseTombstone`
 * never lowers an existing stamp.
 */
export async function persistRemovals(
	userId: string | null,
	ws: string,
	ids: string[],
): Promise<void> {
	if (!isSupported() || ids.length === 0) return;
	const db = await open(userId, ws);
	if (!db) return;
	try {
		const tx = db.transaction(['items', 'meta', 'tombstones'], 'readwrite');
		const store = tx.objectStore('items');
		const meta = (await tx.objectStore('meta').get('sync')) as MetaRow | undefined;
		const tombstones = tx.objectStore('tombstones');
		const cursorSeq = meta ? seqFromCursor(meta.cursor) : undefined;
		for (const id of ids) {
			const stored = (await store.get(id)) as ItemIndexRow | undefined;
			store.delete(id).catch(() => undefined);
			// ALWAYS tombstone, even with no persisted cursor (codex F1): an upsert
			// can write a row before any sync stamps meta.sync, and without a
			// tombstone a delayed snapshot would resurrect that removed row. Stamp
			// the strongest floor available — the persisted cursor if we have one,
			// else the removed row's OWN seq (a snapshot at <= that seq is the same
			// row or older; a genuinely newer create still supersedes), else 0.
			const stamp = cursorSeq ?? (typeof stored?.seq === 'number' ? stored.seq : 0);
			await raiseTombstone(tombstones, id, stamp);
		}
		await tx.done;
	} catch {
		/* swallow — best-effort cache */
	}
}

/**
 * Drop the IDB database entirely. Used by `localIndex.reset` (403
 * full-workspace purge / sign-out) and internally by `hydrate` when
 * the schemaVersion doesn't match. Closes the cached connection
 * first so the delete request isn't blocked by a still-open handle.
 */
export async function wipe(
	userId: string | null,
	ws: string,
): Promise<void> {
	if (!isSupported()) return;
	const k = key(userId, ws);
	const existing = dbs.get(k);
	if (existing) {
		try {
			existing.close();
		} catch {
			/* swallow */
		}
		dbs.delete(k);
	}
	try {
		await new Promise<void>((resolve) => {
			const req = indexedDB.deleteDatabase(dbName(userId, ws));
			req.onsuccess = () => resolve();
			req.onerror = () => resolve();
			req.onblocked = () => resolve();
		});
	} catch {
		/* swallow */
	}
}
