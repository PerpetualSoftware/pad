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
 * not change in that unit, so the constant stayed 3 there. (It is 4 now — the
 * meta row gained `accessEpoch` in IDEA-2898. Two independent counters, and
 * neither implies the other.) A v1 DB reopening under v2 gets the
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
export const LOCAL_INDEX_SCHEMA_VERSION = 4;

/** Result of a `hydrate()` call. Empty payload when there's no cache yet. */
export interface HydrateResult {
	items: ItemIndexRow[];
	cursor: string;
	includesUnparentedMetadata: boolean | null;
	/**
	 * The caller's access fingerprint at the time this cache was written
	 * (IDEA-2898). Persisted for one reason: a revocation that lands while the
	 * tab is CLOSED writes no row, so the cached rows are stale and nothing in
	 * the delta stream says so. Comparing the persisted epoch against the next
	 * response's is what turns that into a resync instead of a silent adopt.
	 *
	 * Null means NO BASELINE, and this build can still WRITE one: a server that
	 * predates `access_epoch` omits it, and `persistDelta`/`persistReplace`
	 * record that absence honestly rather than inventing a value. What the
	 * version bump to 4 rules out is a PRE-IDEA-2898 cache being read as though
	 * it had a baseline — those are wiped, not adopted. Both cases are covered
	 * in localIndexPersistenceAccessEpoch.idb.test.ts.
	 */
	accessEpoch: string | null;
	/**
	 * Did this call actually READ the durable cache? (IDEA-2898, review round 2
	 * of the reduced tip.)
	 *
	 * Every failure here returns the same empty payload as a genuinely empty
	 * cache — deliberately, because a best-effort cache that throws should not
	 * take the app down with it. But "there is nothing stored" and "I could not
	 * find out" are opposite facts for a caller deciding whether it is safe to
	 * adopt a new access epoch, and the empty payload cannot tell them apart.
	 *
	 * TRUE when the read succeeded, AND when IndexedDB is unsupported — with no
	 * durable cache in the picture there is nothing that could later contradict
	 * an adopted epoch. FALSE only when a database that might hold rows could
	 * not be opened or read.
	 */
	durableRead: boolean;
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
	/** See HydrateResult.accessEpoch (IDEA-2898). */
	accessEpoch: string | null;
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
 * Is `cursor` BEHIND the cursor already recorded in `meta.sync`? (TASK-2906.)
 *
 * The durable cursor is the claim that every change at or below it is already
 * reflected in the durable rows. Two functions write that FIELD — `persistDelta`
 * and `persistReplace` — and until this gate both `put` a freshly built
 * `MetaRow` unconditionally, so the cursor was LAST-WRITER-WINS while every
 * other value this cache holds was already ordered (rows arbitrate on `seq`
 * through `resolveRowWrite`, tombstones on `Math.max` through
 * `raiseTombstone`). `persistAccessEpoch` also writes the `sync` ROW, but it
 * carries the stored cursor across unchanged, so it is not a third writer of
 * the position.
 *
 * WHY A REGRESSION IS NOT MERELY WASTEFUL. A cursor that is too LOW only costs
 * a replay, which is why this looked harmless. It is not, because nothing keeps
 * it low until the replay happens: a behind tab lowers the cursor to 400 over
 * rows another tab had current to 500, and then that other tab's next ordinary
 * delta writes 501 — leaving the cache claiming 501 while the rows are missing
 * every change in 401..500, with nothing left that will ever replay them. The
 * regression alone is recoverable; the regression followed by an unrelated
 * advance is not, and the advance is the next ordinary event.
 *
 * A SNAPSHOT'S POSITION IS NEVER LEGITIMATELY BEHIND. `/items-index` returns
 * `max(workspace MAX(seq) read before the list, MAX(returned rows.seq))`, and
 * that floor is workspace-global — unfiltered by visibility and by soft-delete.
 * Every cursor the workspace has issued is therefore at or below its max seq at
 * issue time, and seq is strictly monotonic per workspace. So a `persistReplace`
 * whose cursor is behind is a STALE RESPONSE that landed late, not a resync
 * legitimately pinning the cursor for replay — that pin is about the resyncing
 * tab's own RAM position, where a regression is safe because the merge keeps the
 * newer row per id and a replayed range is idempotent. See TASK-2906
 * checkpoint 2 for the measurement.
 *
 * BUT POSITION IS ALL A BEHIND CURSOR SETTLES (review round 1). A snapshot that
 * is behind in position can still be strictly NEWER IN SCOPE — an access
 * revocation writes no item, so the epoch changes while the cursor does not, and
 * such a snapshot can lose the position race to an unrelated mutation in another
 * tab. Refusing it therefore does drop a true statement, and the reason that is
 * survivable is the refusal being WHOLE: the durable epoch stays at the OLD
 * value, so the disagreement that drives the repair is still on disk. Any
 * hydrate of this cache reads the stale baseline and resyncs; and the tab that
 * WON the position race reads the changed epoch on its own next
 * `/items-changes` response — which carries the live-grant fingerprint even when
 * it carries no changes — and resyncs from a cursor that is not behind. Writing
 * the epoch while withholding the cursor would look like a kinder refusal and is
 * the one thing that would break this: it erases the signal and leaves the
 * revoked rows with nothing left to notice them.
 *
 * ORDERING PRECISION — and why this does NOT go through `seqFromCursor`
 * (review round 2). Both sides of THIS comparison are cursors, which travel as
 * opaque decimal strings and are unrounded until something calls `Number` on
 * them, so an exact compare here is meaningful and is what `compareCursors`
 * does. The lossiness of `ItemIndexRow.seq` — a JSON `number`, already rounded
 * at parse — bounds the ROW comparisons in `resolveRowWrite` and the tombstone
 * stamps in `raiseTombstone`, which is a different pair of values; reasoning
 * from that pair to this one was the first draft's mistake. `seqFromCursor`
 * stays as it is because a tombstone stamp is persisted as a `number` and has
 * to be one.
 *
 * The caller drops the WHOLE batch on true, rows included. A delta's rows and
 * its cursor are one statement, so writing the rows while withholding the
 * cursor would break it in half — and unlike the epoch case that reasoning came
 * from, dropping it whole costs nothing here: everything a behind batch carries
 * is already covered by the claim the stored cursor makes. The batch is
 * superseded, not refused, so no caller is owed a signal or a resync.
 */
function cursorIsBehind(stored: MetaRow | undefined, cursor: string): boolean {
	// No stored cursor: nothing to be behind. Equal is not behind — the rows are
	// a restatement at the same position, and refusing one would drop rows a
	// cold snapshot may hold that the other writer's cache does not.
	if (!stored) return false;
	return compareCursors(cursor, stored.cursor) < 0;
}

/** Read the `sync` meta row, or undefined when this cache has never synced. */
async function readSyncRow(metaStore: {
	get(key: string): Promise<unknown>;
}): Promise<MetaRow | undefined> {
	return (await metaStore.get('sync')) as MetaRow | undefined;
}

/**
 * Order two cursors EXACTLY (TASK-2906, review round 2). Cursors are opaque
 * decimal strings on the wire, so comparing them through `Number` ties any pair
 * that differs only above 2^53 — and a tie reads as "not behind", which is the
 * direction that lets a stale batch through.
 *
 * Canonical non-negative decimals compare by digit count and then
 * lexicographically, which is exact at any magnitude and needs no BigInt.
 * Anything else — an empty string, a non-numeric value, a sign, a fractional
 * part — falls back to `seqFromCursor`'s `Number` reading so malformed input
 * behaves exactly as it did before this function existed rather than acquiring
 * a new meaning here.
 *
 * Returns <0 when `a` is behind `b`, 0 when equal, >0 when ahead.
 */
function compareCursors(a: string, b: string): number {
	if (!DECIMAL_CURSOR.test(a) || !DECIMAL_CURSOR.test(b)) {
		return seqFromCursor(a) - seqFromCursor(b);
	}
	// Strip leading zeros so "007" and "7" compare equal by length as well as
	// by digits; the guard keeps a lone "0" intact.
	const x = a.replace(/^0+(?=\d)/, '');
	const y = b.replace(/^0+(?=\d)/, '');
	if (x.length !== y.length) return x.length - y.length;
	return x < y ? -1 : x > y ? 1 : 0;
}

/** A canonical non-negative decimal cursor — the only shape the server emits. */
const DECIMAL_CURSOR = /^\d+$/;

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
		accessEpoch: null,
		durableRead: false,
		retags: {},
	};
	// Unsupported is not a failed read: there is no durable cache to be wrong
	// about, now or later.
	if (!isSupported()) return { ...empty, durableRead: true };

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
			// A successful read that found an incompatible cache and destroyed
			// it. There is now genuinely nothing stored, so this is `true`.
			return { ...empty, durableRead: true };
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
			accessEpoch: meta?.accessEpoch ?? null,
			durableRead: true,
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
	/**
	 * The scope this batch was fetched under, or `undefined` for CARRY THE
	 * STORED EPOCH (TASK-2909, review round 1 P2).
	 *
	 * TASK-2906 had the caller write an explicit `null` — "cannot know what this
	 * was authorised for" — whenever it could not vouch for the epoch, which is
	 * true at the instant the delta is built but is not safe to WRITE: the
	 * caller cannot vouch during the whole window between a resync installing
	 * its snapshot in RAM and `persistReplace` resolving, and a delta whose
	 * transaction commits after that replace would overwrite the epoch the
	 * replace had just correctly recorded. The next boot then reads a null
	 * baseline over a populated cache and pays a full snapshot to rediscover
	 * what this session already knew.
	 *
	 * Carrying the stored value instead is both safer and more accurate. If the
	 * replace committed, its epoch survives. If it was refused, the stored
	 * epoch still describes the stored ROWS — the two agree, which is the one
	 * property the cache needs — and it is the SERVER's next epoch that
	 * disagrees with it and triggers the resync. Either way the repair happens;
	 * this way it does not also fire when nothing changed.
	 */
	accessEpoch: string | null | undefined,
	removeIds: string[] = [],
): Promise<void> {
	if (!isSupported()) return;
	const db = await open(userId, ws);
	if (!db) return;
	try {
		const tx = db.transaction(['items', 'meta', 'tombstones'], 'readwrite');
		const itemsStore = tx.objectStore('items');
		const tombstones = tx.objectStore('tombstones');
		const metaStore = tx.objectStore('meta');
		// CURSOR ORDERING GATE (TASK-2906). Read the stored row BEFORE any
		// write is issued in this transaction, so a behind batch lands nothing
		// at all — see cursorIsBehind for why the whole batch goes, not just
		// the cursor. The same read supplies the epoch carry-over below.
		const storedMeta = await readSyncRow(metaStore);
		if (cursorIsBehind(storedMeta, cursor)) {
			await tx.done.catch(() => undefined);
			return;
		}
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
		metaStore
			.put({
				key: 'sync',
				cursor,
				schemaVersion: LOCAL_INDEX_SCHEMA_VERSION,
				includesUnparentedMetadata,
				// `undefined` means CARRY THE STORED EPOCH — see the param doc.
				accessEpoch: accessEpoch === undefined ? (storedMeta?.accessEpoch ?? null) : accessEpoch,
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
	accessEpoch: string | null,
): Promise<boolean> {
	// TRUE means "nothing durable can now contradict this snapshot" — the same
	// reading `HydrateResult.durableRead` uses, and the reason an unsupported
	// IndexedDB answers true: with no durable cache in the picture there is no
	// stale rowset for an adopted epoch to describe. FALSE means this call did
	// not put the snapshot on disk, and is deliberately the answer when the
	// database could not even be OPENED — that failure cannot prove a cache is
	// absent, and between "no cache" and "a cache I could not replace" only the
	// second is safe to assume. See the caller notes in
	// `localIndex.ensureAccessScope` and `durableEpochFor` for why the
	// difference is load-bearing (review rounds 2 and 3).
	if (!isSupported()) return true;
	const db = await open(userId, ws);
	if (!db) return false;
	try {
		const tx = db.transaction(['items', 'meta', 'tombstones'], 'readwrite');
		const itemsStore = tx.objectStore('items');
		const metaStore = tx.objectStore('meta');
		// CURSOR ORDERING GATE (TASK-2906), before the clear below issues. A
		// replace whose cursor is behind is a stale response that landed late,
		// not a legitimate pin; letting it through clears rows another tab had
		// current and regresses the cursor over them. Refusing keeps rows this
		// snapshot's scope no longer grants — an accepted cost, recorded on the
		// TASK-2906 trail: it adds no exposure the cache did not already carry
		// one moment earlier, and refusing the epoch ALONG WITH the rows is what
		// keeps the repair alive — see cursorIsBehind's scope paragraph.
		if (cursorIsBehind(await readSyncRow(metaStore), cursor)) {
			await tx.done.catch(() => undefined);
			return false;
		}
		// Queued before the puts; IDB executes requests against a store in
		// issue order, so the clear always lands first.
		itemsStore.clear().catch(() => undefined);
		// The authoritative snapshot supersedes every tombstone and every
		// pending rename: its rows ARE the current truth under the (possibly
		// downgraded) scope, and its slugs are already live (BUG-2601). Drop
		// both overlays so a stale tombstone can't refuse a legitimate row and a
		// stale retag can't regress a newer slug on the next hydrate.
		tx.objectStore('tombstones').clear().catch(() => undefined);
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
				accessEpoch,
			} satisfies MetaRow)
			.catch(() => undefined);
		await tx.done;
		return true;
	} catch {
		/* swallow — best-effort cache */
		return false;
	}
}

/**
 * Record a new access epoch on an EXISTING cache, touching nothing else
 * (IDEA-2898, review of the reduced tip).
 *
 * There is one caller and one reason for it. `ensureAccessScope` may JOIN a
 * resync that is already in flight rather than start one; the resync it joined
 * ran its own `persistReplace` under its own baseline, so the meta row records
 * the OLD epoch while RAM has adopted the told one. Without this the two
 * disagree durably: the session converges, and every reload hydrates the stale
 * baseline and pays a full resync for a scope that has not changed since.
 *
 * Deliberately a no-op when there is no meta row. A cache that has never synced
 * has nothing to describe, and minting a meta row here would invent a cursor —
 * and a cursor is the claim that everything up to it has been seen, which is
 * precisely the claim such a cache cannot make.
 *
 * `expectedPrevious` makes the patch a COMPARE-AND-SET rather than a blind
 * overwrite; see the comment at the check.
 */
export async function persistAccessEpoch(
	userId: string | null,
	ws: string,
	accessEpoch: string | null,
	expectedPrevious: string | null,
): Promise<void> {
	if (!isSupported()) return;
	const db = await open(userId, ws);
	if (!db) return;
	try {
		const tx = db.transaction('meta', 'readwrite');
		const store = tx.objectStore('meta');
		const cached = (await store.get('sync')) as MetaRow | undefined;
		if (!cached) {
			await tx.done.catch(() => undefined);
			return;
		}
		// COMPARE-AND-SET, because this is a read-modify-write on a row other
		// writers own. IDB serializes the transactions, but a `persistDelta` or
		// `persistReplace` carrying a NEWER epoch can commit between the resync
		// this caller joined and this patch — and a blind overwrite would then
		// stamp the older epoch onto rows fetched under the newer one, so the
		// next comparison reports a change that never happened and pays a full
		// resync for it. Patch only the row this caller is actually repairing.
		if ((cached.accessEpoch ?? null) !== expectedPrevious) {
			await tx.done.catch(() => undefined);
			return;
		}
		store.put({ ...cached, accessEpoch } satisfies MetaRow).catch(() => undefined);
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
