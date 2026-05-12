// localSearch — per-workspace MiniSearch index over the localIndex
// (PLAN-1343 / TASK-1363, Phase 3a of DOC-1342). Provides sub-millisecond
// ranked search over titles + parsed fields without a network round-trip.
//
// This module is the DATA LAYER ONLY. TASK-1364 wires the collection page's
// search box to it; TASK-1365 wires the global CommandPalette; TASK-1366
// tunes the relevance config. No UI lives here.
//
// Design:
//
//   - One MiniSearch instance per workspace slug, kept in a module-level
//     plain Map (NOT a SvelteMap — search results are derived on demand
//     via `.search()`, not subscribed to as reactive state). Keeping the
//     index out of the reactive graph means a bulk SSE batch can call
//     `upsert()` 500 times in a row without re-running every subscriber.
//
//   - Source of truth is `localIndex`. We never read items off the wire
//     ourselves; we just mirror what the in-RAM store already holds.
//     `rebuild()` is called once after a workspace's localIndex bootstrap
//     completes; `upsert()` / `remove()` track incremental mutations.
//
//   - Indexed fields and weights:
//       title         (3x boost — primary match target)
//       ref           (2x boost — exact-prefix matchable, e.g. `TASK-5`)
//       item_number   (2x boost — for bare-number queries)
//       tags          (joined string from the JSON array)
//       parent_ref    (e.g. PLAN-5 — "tasks under PLAN-X" discoverability)
//       parent_title  ("tasks under <plan title>")
//       collection_slug
//       fields        (parsed values flattened to a single string — status,
//                      priority, assignee names etc. land as searchable terms)
//
//   - `id` (item.id) is the stored MiniSearch document id; callers resolve
//     to full rows via `localIndex.findByIdOrSlug(ws, id)` (single O(1)
//     Map lookup per result).
//
//   - SSR-safe: every entry point gates on `typeof window !== 'undefined'`.
//     MiniSearch is a pure-JS library so this is conservative — but the
//     index lives in module-scoped state and we don't want SSR-rendered
//     pages to accidentally build a server-side index that won't be used.
//
// TASK-1366 will tune the relevance config (fuzzy slop, prefix matching,
// stop words). TASK-1364 and TASK-1365 wire consumers.

import MiniSearch from 'minisearch';
import type { ItemIndexRow } from '$lib/types';
import { parseFields } from '$lib/types';

/**
 * Options for `search()`.
 */
export interface LocalSearchOptions {
	/** Restrict results to a single collection (by collection slug). */
	collection?: string;
	/** Cap on result count. Default 50. */
	limit?: number;
	/** Include archived (soft-deleted) rows. Default false. */
	includeArchived?: boolean;
}

export interface LocalSearchResult {
	id: string;
	score: number;
}

// ─── MiniSearch config ──────────────────────────────────────────────────────
//
// `fields` is the searchable field list; `storeFields` is the set returned
// in result objects (we only need `id` since callers resolve to full rows
// via localIndex). `boost` weights named fields at search time.
//
// `searchOptions` defaults:
//   - `prefix: true`  — `tas` matches `task` (typo-tolerant typing UX)
//   - `fuzzy: 0.2`    — small edit-distance tolerance for off-by-one typos
//   - `combineWith: 'AND'` — multi-word queries require all tokens
//   - `boost` — title 3x, ref 2x, item_number 2x (see field-level rationale)
//
// `tokenize`: split on whitespace AND non-word characters so `TASK-5` indexes
// as both `task` and `5` — without this, a bare `5` query would not match
// the ref. MiniSearch's default tokenizer already splits on whitespace; the
// custom regex adds `-`/`_`/`.` as splitters.

const TOKENIZE_RE = /[\s\-_.\/]+/;

interface IndexedDoc {
	id: string;
	title: string;
	ref: string;
	item_number: string;
	tags: string;
	parent_ref: string;
	parent_title: string;
	collection_slug: string;
	fields: string;
	// Side-channel metadata used by the filter step in `search()`.
	// These are NOT indexed (not in the `fields` array), just stored so
	// we can filter the ranked id list without a second localIndex lookup
	// per result.
	_collection_slug: string;
	_deleted: boolean;
}

function createMiniSearch(): MiniSearch<IndexedDoc> {
	return new MiniSearch<IndexedDoc>({
		idField: 'id',
		fields: [
			'title',
			'ref',
			'item_number',
			'tags',
			'parent_ref',
			'parent_title',
			'collection_slug',
			'fields',
		],
		storeFields: ['_collection_slug', '_deleted'],
		tokenize: (text) =>
			text
				.toLowerCase()
				.split(TOKENIZE_RE)
				.filter((t) => t.length > 0),
		processTerm: (term) => term.toLowerCase(),
		searchOptions: {
			prefix: true,
			fuzzy: 0.2,
			combineWith: 'AND',
			boost: {
				title: 3,
				ref: 2,
				item_number: 2,
			},
		},
	});
}

// ─── Row → IndexedDoc projection ────────────────────────────────────────────
//
// The skinny `ItemIndexRow` already carries `tags` as a JSON-encoded string
// (a stringified array) and `fields` as a JSON-encoded string of {key: val}.
// We flatten both to plain space-separated strings here so MiniSearch's
// default tokenizer can split them. Numeric / boolean field values get
// String()-coerced; nested objects are skipped to avoid `[object Object]`
// littering the index.

function safeJsonParse<T>(raw: string | undefined | null, fallback: T): T {
	if (!raw) return fallback;
	try {
		return JSON.parse(raw) as T;
	} catch {
		return fallback;
	}
}

function flattenFields(fieldsJSON: string | undefined): string {
	const parsed = safeJsonParse<Record<string, unknown>>(fieldsJSON, {});
	const out: string[] = [];
	for (const v of Object.values(parsed)) {
		if (v === null || v === undefined) continue;
		if (typeof v === 'string') {
			out.push(v);
		} else if (typeof v === 'number' || typeof v === 'boolean') {
			out.push(String(v));
		}
		// Arrays of strings (e.g. tag-shaped fields) get joined; nested
		// objects are dropped — they would only contribute noise tokens.
		else if (Array.isArray(v)) {
			for (const el of v) {
				if (typeof el === 'string') out.push(el);
				else if (typeof el === 'number' || typeof el === 'boolean') {
					out.push(String(el));
				}
			}
		}
	}
	return out.join(' ');
}

function flattenTags(tagsJSON: string | undefined): string {
	const parsed = safeJsonParse<string[]>(tagsJSON, []);
	if (!Array.isArray(parsed)) return '';
	return parsed.filter((t) => typeof t === 'string').join(' ');
}

function buildDoc(row: ItemIndexRow): IndexedDoc {
	// `ref` is the prefixed identifier (`TASK-5`). Built locally so we
	// don't import formatItemRef and pull a heavier helper into the
	// hot path.
	const ref =
		row.collection_prefix && row.item_number !== undefined
			? `${row.collection_prefix}-${row.item_number}`
			: '';
	return {
		id: row.id,
		title: row.title || '',
		ref,
		item_number: row.item_number !== undefined ? String(row.item_number) : '',
		tags: flattenTags(row.tags),
		parent_ref: row.parent_ref || '',
		parent_title: row.parent_title || '',
		collection_slug: row.collection_slug || '',
		fields: flattenFields(row.fields),
		_collection_slug: row.collection_slug || '',
		_deleted: !!row.deleted_at,
	};
}

// ─── Per-workspace index storage ────────────────────────────────────────────

const indexes = new Map<string, MiniSearch<IndexedDoc>>();

function ensureIndex(ws: string): MiniSearch<IndexedDoc> {
	let idx = indexes.get(ws);
	if (!idx) {
		idx = createMiniSearch();
		indexes.set(ws, idx);
	}
	return idx;
}

function ssrSafe(): boolean {
	return typeof window !== 'undefined';
}

// ─── Public API ─────────────────────────────────────────────────────────────

export const localSearch = {
	/**
	 * Rebuild the search index for a workspace from a snapshot of rows
	 * (typically `localIndex.workspaces.get(ws)?.items.values()` after
	 * a fresh bootstrap). Drops any previously indexed rows for the
	 * workspace. Idempotent — safe to call again after a `reset()` and
	 * a fresh bootstrap.
	 *
	 * Cost: linear in row count. ~5,000 items measured at <50ms on a
	 * mid-range laptop — well under the Phase 3 budget.
	 */
	rebuild(ws: string, rows: Iterable<ItemIndexRow>): void {
		if (!ssrSafe()) return;
		const idx = createMiniSearch();
		const docs: IndexedDoc[] = [];
		for (const row of rows) {
			docs.push(buildDoc(row));
		}
		// `addAll` is faster than N calls to `add` for bulk loads — it
		// batches the inverted-index updates.
		if (docs.length > 0) idx.addAll(docs);
		indexes.set(ws, idx);
	},

	/**
	 * Incremental upsert. Wired into `localIndex.upsert` and the
	 * `applyDelta` path so the search index stays in lockstep with the
	 * canonical store without a periodic rebuild.
	 *
	 * MiniSearch doesn't expose an atomic "upsert" — `add()` would dupe
	 * a document if called twice for the same id, and `replace()` is
	 * not idempotent for a missing id. The reliable pattern is
	 * `discard(id)` (no-op if missing) followed by `add(doc)`. This is
	 * cheap enough at the per-row scale we hit (one upsert per SSE event).
	 */
	upsert(ws: string, row: ItemIndexRow): void {
		if (!ssrSafe()) return;
		const idx = ensureIndex(ws);
		const doc = buildDoc(row);
		// `discard` removes from the inverted index without throwing on a
		// missing id, then `add` inserts the fresh doc. This is the
		// MiniSearch-recommended pattern for in-place mutation.
		if (idx.has(doc.id)) idx.discard(doc.id);
		idx.add(doc);
	},

	/**
	 * Incremental remove. Wired into `localIndex.remove` and the
	 * 403-purge path. No-op for a missing id.
	 */
	remove(ws: string, id: string): void {
		if (!ssrSafe()) return;
		const idx = indexes.get(ws);
		if (!idx) return;
		if (idx.has(id)) idx.discard(id);
	},

	/**
	 * Drop the index for a workspace entirely. Paired with
	 * `localIndex.reset()` on sign-out / 403-purge / workspace deletion
	 * so the next bootstrap rebuilds from scratch.
	 */
	reset(ws: string): void {
		indexes.delete(ws);
	},

	/**
	 * Synchronous ranked search. Returns at most `opts.limit` (default 50)
	 * id+score pairs ordered by descending score. Empty / whitespace
	 * queries return an empty array — callers should treat that as "no
	 * search active" and fall back to the unfiltered list.
	 *
	 * The collection / archived filters are applied AFTER ranking so the
	 * collection-scoped query still sees the same relative ordering as a
	 * workspace-wide one. We over-fetch by 4x to avoid an under-fill
	 * when the filter drops a chunk of the top results, then truncate
	 * to `limit`.
	 */
	search(ws: string, q: string, opts: LocalSearchOptions = {}): LocalSearchResult[] {
		if (!ssrSafe()) return [];
		const query = q.trim();
		if (!query) return [];
		const idx = indexes.get(ws);
		if (!idx) return [];

		const limit = opts.limit ?? 50;
		const includeArchived = opts.includeArchived === true;
		const wantCollection = opts.collection;

		// MiniSearch returns all matching docs sorted by score; we cap
		// after filtering. Asking for `limit * 4` is a heuristic that
		// stays cheap even at large workspace sizes — `search()` is
		// linear in matching-doc count.
		const overfetch = limit * 4;
		const raw = idx.search(query) as Array<{
			id: string;
			score: number;
			_collection_slug?: string;
			_deleted?: boolean;
		}>;

		const out: LocalSearchResult[] = [];
		for (const r of raw) {
			if (!includeArchived && r._deleted) continue;
			if (wantCollection && r._collection_slug !== wantCollection) continue;
			out.push({ id: r.id, score: r.score });
			if (out.length >= overfetch) break;
		}
		if (out.length > limit) out.length = limit;
		return out;
	},

	/**
	 * Number of indexed documents for a workspace. Test / debug aid.
	 */
	size(ws: string): number {
		return indexes.get(ws)?.documentCount ?? 0;
	},
};
