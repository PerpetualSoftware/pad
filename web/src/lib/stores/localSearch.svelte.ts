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
import { SvelteMap } from 'svelte/reactivity';
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

/**
 * Structured query produced by `parseSearchQuery`. Consumers (collection
 * page, CommandPalette) destructure to decide what to dispatch:
 *
 *   - `body`: true → server FTS path (local index excludes `content`)
 *   - `collection`: scope the local search to one collection
 *   - `itemNumber`: exact-number lookup (#5 / item:5 / bare digits)
 *   - `ref`: PREFIX-N pattern detected; the matched doc is hoisted to
 *     the top of `search()` results
 *   - `text`: residual query after prefix stripping; pass to `search()`
 *
 * NOTE: an `is:archived` prefix was prototyped but pulled before ship:
 * the server `/search` endpoint hard-filters `deleted_at IS NULL`, so
 * combining it with `body:` would silently drop archived hits. The
 * existing `showArchived` UI toggle is the supported path until the
 * server endpoint grows `include_archived` support.
 */
export interface ParsedSearchQuery {
	text: string;
	body: boolean;
	collection?: string;
	itemNumber?: number;
	ref?: string;
}

const PREFIX_BODY_RE = /^(?:body|content):/i;
const PREFIX_COLL_RE = /^coll:(.+)$/i;
const PREFIX_ITEM_NUMBER_RE = /^(?:#|item:)(\d+)$/i;
const REF_PATTERN_RE = /^([A-Za-z]+)-(\d+)$/;

/**
 * Parse a free-form search query into structured prefixes + residual
 * text. Unknown / malformed prefixes pass through as part of `text` so
 * the user always sees something happen.
 *
 * Examples:
 *   "TASK-5"               → { ref: "TASK-5", text: "TASK-5", ... }
 *   "#5"                   → { itemNumber: 5, text: "", ... }
 *   "body:foo"             → { body: true, text: "foo", ... }
 *   "coll:tasks migrate"   → { collection: "tasks", text: "migrate", ... }
 *
 * Exported so the collection page and CommandPalette share one parser;
 * also makes the prefix UX testable in isolation.
 */
export function parseSearchQuery(raw: string): ParsedSearchQuery {
	const out: ParsedSearchQuery = { text: '', body: false };
	const tokens = raw.trim().split(/\s+/).filter(Boolean);
	const residual: string[] = [];
	for (const tok of tokens) {
		// body:/content: — strip prefix; any trailing chars (`body:foo`)
		// become residual text. A bare `body:` keeps the flag and leaves
		// residual untouched so the user can keep typing.
		if (PREFIX_BODY_RE.test(tok)) {
			out.body = true;
			const rest = tok.slice(tok.indexOf(':') + 1);
			if (rest) residual.push(rest);
			continue;
		}
		const collMatch = tok.match(PREFIX_COLL_RE);
		if (collMatch) {
			out.collection = collMatch[1].toLowerCase();
			continue;
		}
		const itemMatch = tok.match(PREFIX_ITEM_NUMBER_RE);
		if (itemMatch) {
			const n = Number(itemMatch[1]);
			if (Number.isFinite(n)) out.itemNumber = n;
			continue;
		}
		const refMatch = tok.match(REF_PATTERN_RE);
		if (refMatch) {
			// `TASK-5` is BOTH a ref and a searchable token — keep it in
			// residual so prefix/fuzz matching still works (the user may
			// have typed it as plain text), and also expose `ref` so
			// `search()` can hoist the exact match.
			out.ref = `${refMatch[1].toUpperCase()}-${refMatch[2]}`;
		}
		residual.push(tok);
	}
	out.text = residual.join(' ');
	return out;
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
// `tokenize`: split on anything that isn't a letter or digit (in any
// unicode script — handles non-ASCII titles too). This is broader than
// MiniSearch's default `SPACE_OR_PUNCTUATION` regex but in the same
// shape: it covers whitespace, dashes, dots, slashes, colons, commas,
// parens, brackets, etc., so `TASK-5` → ['task', '5'], `foo,bar` →
// ['foo', 'bar'], `Item (Done)` → ['item', 'done']. Underscore counts
// as a splitter too so snake_case field keys index as separate tokens.
// Codex review round 1 (TASK-1363) caught that the original narrow
// split missed `foo:bar` / `foo,bar` / `foo(bar)` cases.

const TOKENIZE_RE = /[^\p{L}\p{N}]+/u;

// Per-term fuzz / prefix tuning (TASK-1367 / Phase 3e). Short terms
// fuzzed against an index of 5,000 rows produce too many low-quality
// matches — `cat` shouldn't match `bat`/`hat`/`category` via fuzz when
// the user typed a complete 3-letter word. Empirically:
//   - len 1: no fuzz, no prefix (single chars match too much noise)
//   - len 2-3: no fuzz, prefix on (`db` prefix-matches `database`)
//   - len 4+: fuzz=0.2, prefix on (one-edit typo tolerance kicks in)
// The cutoffs are conservative — bumping the floor higher costs felt
// recall; lowering it reintroduces the `cat`→`category` fuzz problem.

function termFuzzy(term: string): number | false {
	return term.length >= 4 ? 0.2 : false;
}

function termPrefix(term: string): boolean {
	return term.length >= 2;
}

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
	// per result. `_ref` / `_item_number` (TASK-1367) carry the
	// uppercased ref string and numeric item_number to power the
	// exact-ref short-circuit and `#5`/`item:5` syntax.
	_collection_slug: string;
	_deleted: boolean;
	_ref: string;
	_item_number: number;
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
		storeFields: ['_collection_slug', '_deleted', '_ref', '_item_number'],
		tokenize: (text) =>
			text
				.toLowerCase()
				.split(TOKENIZE_RE)
				.filter((t) => t.length > 0),
		processTerm: (term) => term.toLowerCase(),
		searchOptions: {
			prefix: termPrefix,
			fuzzy: termFuzzy,
			combineWith: 'AND',
			// Title boost was 3x → 5x (TASK-1367): empirical eyeball test
			// showed title-vs-tag ties leaving tag-rich rows ahead of
			// closer title matches. Ref / item_number boost was 2x → 4x
			// so that a typed prefix-shaped ref query out-scores
			// incidental field hits even before the exact-ref short-circuit
			// in `search()` prepends the row.
			boost: {
				title: 5,
				ref: 4,
				item_number: 4,
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
		_ref: ref.toUpperCase(),
		_item_number: row.item_number ?? 0,
	};
}

// ─── Per-workspace index storage ────────────────────────────────────────────

const indexes = new Map<string, MiniSearch<IndexedDoc>>();

// `epochs` is a reactive per-workspace counter bumped on every index
// mutation. Consumers (collection page, CommandPalette) read it inside
// a Svelte 5 `$effect` to re-derive `searchResultIds` automatically
// when the underlying index changes — without this, an SSE-driven
// `localIndex.upsert` would update the canonical row list but the
// search filter Set would go stale and a freshly-matching item could
// stay hidden, or an edited item that no longer matches could stay
// visible until the user retyped the query (Codex round 3 P2 of
// TASK-1364). Reactive `SvelteMap` so per-workspace reads are tracked.
const epochs = new SvelteMap<string, number>();

function bumpEpoch(ws: string): void {
	epochs.set(ws, (epochs.get(ws) ?? 0) + 1);
}

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

/**
 * Exact item_number lookup. Used by the `#N` / `item:N` / bare-digit
 * paths in `search()`. MiniSearch's tokenized search would surface
 * every doc whose token list contains `N` (and prefix-on widens that
 * to every `N*` doc) — for item-number intent we want a single doc.
 *
 * Walks the stored `_item_number` field via `idx.search` with a
 * `filter`. Returns at most one result; the caller still applies
 * collection / archived gates on the way out.
 */
function exactItemNumberLookup(
	idx: MiniSearch<IndexedDoc>,
	n: number,
	opts: { limit: number; includeArchived: boolean; wantCollection?: string },
): LocalSearchResult[] {
	// `idx.search` with `filter` is the cheapest way to walk every doc
	// — we pass a query string that always returns the full index
	// (an empty-token search via `*` isn't supported, so we use the
	// number as the query AND filter for exact match). This keeps the
	// cost linear in matching docs rather than full index size.
	const raw = idx.search(String(n), {
		filter: (r) => (r as unknown as IndexedDoc)._item_number === n,
	}) as Array<{
		id: string;
		score: number;
		_collection_slug?: string;
		_deleted?: boolean;
	}>;
	const out: LocalSearchResult[] = [];
	for (const r of raw) {
		if (!opts.includeArchived && r._deleted) continue;
		if (opts.wantCollection && r._collection_slug !== opts.wantCollection) {
			continue;
		}
		out.push({ id: r.id, score: r.score });
		if (out.length >= opts.limit) break;
	}
	return out;
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
		bumpEpoch(ws);
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
		bumpEpoch(ws);
	},

	/**
	 * Incremental remove. Wired into `localIndex.remove` and the
	 * 403-purge path. No-op for a missing id.
	 */
	remove(ws: string, id: string): void {
		if (!ssrSafe()) return;
		const idx = indexes.get(ws);
		if (!idx) return;
		if (idx.has(id)) {
			idx.discard(id);
			bumpEpoch(ws);
		}
	},

	/**
	 * Drop the index for a workspace entirely. Paired with
	 * `localIndex.reset()` on sign-out / 403-purge / workspace deletion
	 * so the next bootstrap rebuilds from scratch.
	 */
	reset(ws: string): void {
		indexes.delete(ws);
		epochs.delete(ws);
	},

	/**
	 * Reactive per-workspace mutation epoch. Bumped on every successful
	 * `rebuild` / `upsert` / `remove`. Consumers should READ this inside
	 * a `$effect` so their derived state (e.g. a `searchResultIds` Set
	 * that intersects local items with a query) re-runs whenever the
	 * underlying index changes — Codex round 3 P2 of TASK-1364.
	 */
	epoch(ws: string): number {
		return epochs.get(ws) ?? 0;
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

		const parsed = parseSearchQuery(query);

		const limit = opts.limit ?? 50;
		const includeArchived = opts.includeArchived === true;
		const wantCollection = opts.collection ?? parsed.collection;

		// Bare-number / explicit item-number queries: short-circuit to an
		// exact item_number lookup so a typed `5` or `#5` lands the right
		// row first instead of returning every doc whose token list
		// contains a `5`. The task explicitly calls this out: "5 alone
		// shouldn't return everything matching anywhere; require the
		// prefix (TASK-5) OR explicit #5 / item:5 syntax". We accept all
		// three: TASK-5, #5/item:5, and bare digits — bare digits get
		// treated as #N for typing-speed (Codex round 2 of TASK-1367
		// caught that the prior `!parsed.text` guard suppressed the
		// bare-digit branch because `parseSearchQuery("5")` leaves
		// "5" in `parsed.text`). The result is a single doc (or empty)
		// which we still post-filter by collection / archived so the
		// caller's scope is honored.
		const bareDigit = /^\d+$/.test(query) ? Number(query) : null;
		if (parsed.itemNumber !== undefined && !parsed.text && !parsed.ref) {
			return exactItemNumberLookup(idx, parsed.itemNumber, {
				limit,
				includeArchived,
				wantCollection,
			});
		}
		if (bareDigit !== null) {
			return exactItemNumberLookup(idx, bareDigit, {
				limit,
				includeArchived,
				wantCollection,
			});
		}

		// MiniSearch returns all matching docs sorted by score; we cap
		// after filtering. Asking for `limit * 4` is a heuristic that
		// stays cheap even at large workspace sizes — `search()` is
		// linear in matching-doc count.
		const overfetch = limit * 4;
		// Pass the residual text (post-prefix-strip) to MiniSearch. If
		// the user typed nothing but prefixes (`coll:tasks` with no
		// query, `is:archived` with no query) return empty — Codex
		// round 1 of TASK-1367 caught that the prior `parsed.text ||
		// query` fallback searched the literal `is`/`archived` /
		// `coll`/`tasks` tokens, which surfaced unrelated rows.
		const searchText = parsed.text;
		if (!searchText.trim()) return [];

		const raw = idx.search(searchText) as Array<{
			id: string;
			score: number;
			_collection_slug?: string;
			_deleted?: boolean;
			_ref?: string;
			_item_number?: number;
		}>;

		// Exact-ref hoist: if the query named a ref (`TASK-5`), pull the
		// matching doc to the front with a synthetic high score. The
		// doc may not even be the top MiniSearch hit (a longer title
		// match against a sibling could outscore it) — exact-ref intent
		// is unambiguous, so we promote.
		const wantRef = parsed.ref;
		let hoisted: LocalSearchResult | null = null;
		const out: LocalSearchResult[] = [];
		for (const r of raw) {
			if (!includeArchived && r._deleted) continue;
			if (wantCollection && r._collection_slug !== wantCollection) continue;
			if (wantRef && !hoisted && r._ref === wantRef) {
				// `score: Infinity` would render as a weird display but
				// the caller only uses score for relative ordering; use
				// a synthetic value that beats anything MiniSearch can
				// produce in practice.
				hoisted = { id: r.id, score: r.score + 1e6 };
				continue;
			}
			out.push({ id: r.id, score: r.score });
			if (out.length >= overfetch) break;
		}
		if (hoisted) out.unshift(hoisted);
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
